package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/alpine-hodler/sherpa/internal/web"
	"github.com/alpine-hodler/sherpa/internal/web/auth"
	"github.com/alpine-hodler/sherpa/internal/web/coinbasepro"
	"github.com/alpine-hodler/sherpa/pkg/proto"
	"github.com/alpine-hodler/sherpa/pkg/repository"
	"github.com/alpine-hodler/sherpa/pkg/storage"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// APIKey is one method of HTTP(s) transport that requires a passphrase, key, and secret.
type APIKey struct {
	Passphrase string `yaml:"passphrase"`
	Key        string `yaml:"key"`
	Secret     string `yaml:"secret"`
}

// Authentication is the credential information to be used to construct an HTTP(s) transport for accessing the API.
type Authentication struct {
	APIKey *APIKey `yaml:"apiKey"`
}

type timeseries struct {
	StartName string `yaml:"startName"`
	EndName   string `yaml:"endName"`

	// Period is the size of each chunk in seconds for which we can query the API. Some API will not allow us to
	// query all data within the start and end range.
	Period int32 `yaml:"period"`

	// Layout is the time layout for parsing the "Start" and "End" values into "time.Time". The default is assumed
	// to be RFC3339.
	Layout *string `yaml:"layout"`
}

// chunks will attempt to use the query string of a URL to partition the timeseries into "chunks" of time for queying
// a web API.
func (ts *timeseries) chunks(url *url.URL) ([][2]time.Time, error) {
	chunks := [][2]time.Time{}

	// If layout is not set, then default it to be RFC3339
	if ts.Layout == nil {
		str := time.RFC3339
		ts.Layout = &str
	}

	query := url.Query()
	startSlice := query[ts.StartName]
	if len(startSlice) != 1 {
		return nil, fmt.Errorf("'startName' is required for timeseries data")
	}

	start, err := time.Parse(*ts.Layout, startSlice[0])
	if err != nil {
		return nil, err
	}

	endSlice := query[ts.EndName]
	if len(endSlice) != 1 {
		return nil, fmt.Errorf("'endName' is required for timeseries data")
	}

	end, err := time.Parse(*ts.Layout, endSlice[0])
	if err != nil {
		return nil, err
	}

	for start.Before(end) {
		next := start.Add(time.Second * time.Duration(ts.Period))
		if next.Before(end) {
			chunks = append(chunks, [2]time.Time{start, next})
		} else {
			chunks = append(chunks, [2]time.Time{start, end})
		}
		start = next
	}
	return chunks, nil
}

// Request is the information needed to query the web API for data to transport.
type Request struct {
	// Method is the HTTP(s) method used to construct the http request to fetch data for storage.
	Method string `yaml:"method"`

	// Endpoint is the fragment of the URL that will be used to request data from the API. This value can include
	// query parameters.
	Endpoint string `yaml:"endpoint"`

	// RateLimitBurstCap represents the number of requests that can be made per second to the endpoint. The
	// value of this should come from the documentation in the underlying API.
	RateLimitBurstCap int `yaml:"ratelimit"`

	// Query represent the query params to apply to the URL generated by the request.
	Query map[string]string

	// Timeseries indicates that the underlying data should be queries as a time series. This means that the
	Timeseries *timeseries `yaml:"timeseries"`

	// Table is the name of the table/collection to insert the data fetched from the web API.
	Table *string
}

// RateLimitConfig is the data needed for constructing a rate limit for the HTTP requests.
type RateLimitConfig struct {
	// Burst represents the number of requests that we limit over a period frequency.
	Burst *int `yaml:"burst"`

	// Period is the number of times to allow a burst per second.
	Period *time.Duration `yaml:"period"`
}

func (rl RateLimitConfig) validate() error {
	wrapper := func(field string) error {
		return fmt.Errorf("%q is a required field on transport.RateLimitConfig", field)
	}
	if rl.Burst == nil {
		return wrapper("Burst")
	}
	if rl.Period == nil {
		return wrapper("Period")
	}
	return nil
}

// Config is the configuration used to query data from the web using HTTP requests and storing that data using
// the repositories defined by the "DNSList".
type Config struct {
	URL             string           `yaml:"url"`
	Authentication  Authentication   `yaml:"authentication"`
	DNSList         []string         `yaml:"dnsList"`
	Requests        []*Request       `yaml:"requests"`
	RateLimitConfig *RateLimitConfig `yaml:"rateLimit"`

	Logger   *logrus.Logger
	Truncate bool
}

// connect will attempt to connect to the web API client. Since there are multiple ways to build a transport given the
// authentication data, this method will exhuast every transport option in the "Authentication" struct.
func (cfg *Config) connect(ctx context.Context) (*web.Client, error) {
	if apiKey := cfg.Authentication.APIKey; apiKey != nil {
		return web.NewClient(ctx, auth.NewAPIKey().
			SetURL(cfg.URL).
			SetKey(apiKey.Key).
			SetPassphrase(apiKey.Passphrase).
			SetSecret(apiKey.Secret))
	}
	return nil, nil
}

// repositories will return a slice of generic repositories for upserting.
func (cfg *Config) repositories(ctx context.Context) ([]repository.Generic, error) {
	repos := []repository.Generic{}
	for _, dns := range cfg.DNSList {
		stg, err := storage.New(ctx, dns)
		if err != nil {
			return nil, fmt.Errorf("error building repositories for transport config: %v", err)
		}
		repos = append(repos, repository.New(ctx, stg))
	}
	return repos, nil
}

func (cfg *Config) validate() error {
	wrapper := func(field string) error { return fmt.Errorf("%q is a required field on transport.Config", field) }
	if cfg.RateLimitConfig == nil {
		return wrapper("RateLimitConfig")
	}
	if err := cfg.RateLimitConfig.validate(); err != nil {
		return err
	}
	return nil
}

// newFetchConfig constructs a new HTTP request.
func newFetchConfig(ctx context.Context, cfg *Config, req *Request, client *web.Client,
	rl *rate.Limiter) (*web.FetchConfig, error) {

	rawURL, err := url.JoinPath(cfg.URL, req.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("error joining url %q to endpoint %q: %v", cfg.URL, req.Endpoint, err)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("error parsing raw url %q: %v", rawURL, err)
	}

	// Apply the query parameters if they are on the request.
	if req.Query != nil {
		query := u.Query()
		for n, v := range req.Query {
			query.Set(n, v)
		}
		u.RawQuery = query.Encode()
	}

	webcfg := &web.FetchConfig{
		Client:      client,
		Method:      req.Method,
		URL:         u,
		RateLimiter: rl,
	}

	return webcfg, nil
}

type repoJob struct {
	b     []byte
	url   *url.URL
	table *string
}

type repoConfig struct {
	repositories []repository.Generic
	jobs         <-chan *repoJob
	done         chan bool
	logger       *logrus.Logger
	truncate     bool
}

func repositoryWorker(ctx context.Context, id int, cfg *repoConfig) {
	for job := range cfg.jobs {
		// ? Should we put the repos in a worker and run them concurrently as well?

		endpoint := strings.TrimPrefix(job.url.EscapedPath(), "/")
		endpointParts := strings.Split(endpoint, "/")

		table := endpointParts[len(endpointParts)-1]
		if job.table != nil {
			table = *job.table
		}

		var encodingCallback func(*repoJob) ([]byte, error)

		// Some endpoints for some hosts require special logic.
		switch table {
		case "candles":
			if strings.Contains(job.url.Host, "coinbase.com") {
				granularity := job.url.Query()["granularity"][0]
				switch granularity {
				case "60":
					table = "candle_minutes"
				}

				productID := endpointParts[1]
				encodingCallback = func(job *repoJob) ([]byte, error) {
					var candles coinbasepro.Candles
					if err := json.Unmarshal(job.b, &candles); err != nil {
						return nil, err
					}
					for _, candle := range candles {
						candle.ProductID = productID
					}
					return json.Marshal(candles)
				}
			}
		default:
			encodingCallback = func(job *repoJob) ([]byte, error) {
				return job.b, nil
			}
		}

		for _, repo := range cfg.repositories {

			bytes, err := encodingCallback(job)
			if err != nil {
				cfg.logger.Fatal(err)
			}
			rsp := new(proto.CreateResponse)
			if err := repo.UpsertJSON(ctx, table, bytes, rsp); err != nil {
				cfg.logger.Fatal(err)
			}

			cfg.logger.Infof("upsert completed: (id=%v) %s", id, table)
		}
		cfg.done <- true
	}
}

// flattenedRequest contains all of the request information to create a web job. The number of flattened request
// for an operation should be 1-1 with the number of requests to the web API.
type flattenedRequest struct {
	fetchConfig *web.FetchConfig
	table       *string
}

type webWorkerJob struct {
	*flattenedRequest
	repoJobs chan<- *repoJob
	client   *web.Client
	logger   *logrus.Logger
}

func webWorker(ctx context.Context, id int, jobs <-chan *webWorkerJob) {
	for job := range jobs {
		bytes, err := web.Fetch(ctx, job.fetchConfig)
		if err != nil {
			job.logger.Fatal(err)
		}
		job.repoJobs <- &repoJob{b: bytes, url: job.fetchConfig.URL, table: job.table}
		job.logger.Infof("web fetch completed: (id=%v) %s", id, job.fetchConfig.URL.Path)
	}
}

// Upsert will use the configuration file to upsert data from the
func Upsert(ctx context.Context, cfg *Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	client, err := cfg.connect(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to client: %v", err)
	}
	cfg.Logger.Info("connection establed")

	// ? how do we make this a limited buffer?
	repoJobCh := make(chan *repoJob)

	repos, err := cfg.repositories(ctx)
	if err != nil {
		return err
	}

	// create a rate limiter to pass to all "flattenedRequest". This has to be defined outside of the scope of
	// individual "flattenedRequest"s so that they all share the same rate limiter, even concurrent requests to
	// different endpoints could cause a rate limit error on a web API.
	rateLimiter := rate.NewLimiter(rate.Every(*cfg.RateLimitConfig.Period*time.Second), *cfg.RateLimitConfig.Burst)

	// Get all of the fetch configurations needed to process the upsert.
	var flattenedRequests []*flattenedRequest
	for _, req := range cfg.Requests {
		fetchConfig, err := newFetchConfig(ctx, cfg, req, client, rateLimiter)
		if err != nil {
			return err
		}

		if timeseries := req.Timeseries; timeseries != nil {
			xurl := fetchConfig.URL
			chunks, err := timeseries.chunks(xurl)
			if err != nil {
				return fmt.Errorf("error getting timeseries chunks: %v", chunks)
			}
			for _, chunk := range chunks {
				// copy the request and update it to reflect the partitioned timeseries
				chunkReq := req
				chunkReq.Query[timeseries.StartName] = chunk[0].Format(*timeseries.Layout)
				chunkReq.Query[timeseries.EndName] = chunk[1].Format(*timeseries.Layout)

				chunkedFetchConfig, err := newFetchConfig(ctx, cfg, chunkReq, client, rateLimiter)
				if err != nil {
					return err
				}
				flattenedRequests = append(flattenedRequests, &flattenedRequest{
					fetchConfig: chunkedFetchConfig,
					table:       req.Table,
				})

			}
		} else {
			flattenedRequests = append(flattenedRequests, &flattenedRequest{
				fetchConfig: fetchConfig,
				table:       req.Table,
			})
		}
	}

	repoWorkerCfg := &repoConfig{
		repositories: repos,
		logger:       cfg.Logger,
		done:         make(chan bool, len(flattenedRequests)),
		jobs:         repoJobCh,
		truncate:     cfg.Truncate,
	}

	for id := 1; id <= runtime.NumCPU(); id++ {
		go repositoryWorker(ctx, id, repoWorkerCfg)
	}
	cfg.Logger.Info("repository workers started")

	webWorkerJobs := make(chan *webWorkerJob, len(cfg.Requests))

	// Start the same number of web workers as the cores on the machine.
	for id := 1; id <= runtime.NumCPU(); id++ {
		go webWorker(ctx, id, webWorkerJobs)
	}
	cfg.Logger.Info("web workers started")

	// Enqueue the worker jobs
	for _, req := range flattenedRequests {
		webWorkerJobs <- &webWorkerJob{
			flattenedRequest: req,
			repoJobs:         repoJobCh,
			client:           client,
			logger:           cfg.Logger,
		}
	}

	cfg.Logger.Info("web worker jobs enqueued")

	// Wait for all of the data to flush.
	for a := 1; a <= len(flattenedRequests); a++ {
		<-repoWorkerCfg.done
	}
	cfg.Logger.Info("repository workers finished")

	return nil
}

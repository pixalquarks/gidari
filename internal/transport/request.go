// Copyright 2022 The Gidari Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
package transport

import (
	"fmt"
	"net/url"
	"path"

	"github.com/alpine-hodler/gidari/internal/web"
	"golang.org/x/time/rate"
)

// Request is the information needed to query the web API for data to transport.
type Request struct {
	// Method is the HTTP(s) method used to construct the http request to fetch data for storage.
	Method string `yaml:"method"`

	// Endpoint is the fragment of the URL that will be used to request data from the API. This value can include
	// query parameters.
	Endpoint string `yaml:"endpoint"`

	// Query represent the query params to apply to the URL generated by the request.
	Query map[string]string

	// Timeseries indicates that the underlying data should be queries as a time series. This means that the
	Timeseries *timeseries `yaml:"timeseries"`

	// Table is the name of the table/collection to insert the data fetched from the web API.
	Table string `yaml:"table"`

	// Truncate before upserting on single request
	Truncate *bool `yaml:"truncate"`

	//
	RateLimitConfig *RateLimitConfig `yaml:"rate_limit"`
}

// newFetchConfig will constrcut a new HTTP request from the transport request.
func (req *Request) newFetchConfig(rurl url.URL, client *web.Client) *web.FetchConfig {
	rurl.Path = path.Join(rurl.Path, req.Endpoint)

	// Add the query params to the URL.
	if req.Query != nil {
		query := rurl.Query()
		for key, value := range req.Query {
			query.Set(key, value)
		}

		rurl.RawQuery = query.Encode()
	}

	// create a rate limiter to pass to all "flattenedRequest". This has to be defined outside of the scope of
	// individual "flattenedRequest"s so that they all share the same rate limiter, even concurrent requests to
	// different endpoints could cause a rate limit error on a web API.
	rateLimiter := rate.NewLimiter(rate.Every(*req.RateLimitConfig.Period), *req.RateLimitConfig.Burst)

	return &web.FetchConfig{
		Method:      req.Method,
		URL:         &rurl,
		C:           client,
		RateLimiter: rateLimiter,
	}
}

// flattenedRequest contains all of the request information to create a web job. The number of flattened request  for an
// operation should be 1-1 with the number of requests to the web API.
type flattenedRequest struct {
	fetchConfig *web.FetchConfig
	table       string
}

// flatten will compress the request information into a "web.FetchConfig" request and a "table" name for storage
// interaction.
func (req *Request) flatten(rurl url.URL, client *web.Client) *flattenedRequest {
	fetchConfig := req.newFetchConfig(rurl, client)

	return &flattenedRequest{
		fetchConfig: fetchConfig,
		table:       req.Table,
	}
}

// flattenTimeseries will compress the request information into a "web.FetchConfig" request and a "table" name for
// storage interaction. This function will create a flattened request for each time series in the request. If no
// timeseries are defined, this function will return a single flattened request.
func (req *Request) flattenTimeseries(rurl url.URL, client *web.Client) ([]*flattenedRequest, error) {
	timeseries := req.Timeseries
	if timeseries == nil {
		flatReq := req.flatten(rurl, client)

		return []*flattenedRequest{flatReq}, nil
	}

	requests := make([]*flattenedRequest, 0, len(timeseries.chunks))

	// Add the query params to the URL.
	if req.Query != nil {
		query := rurl.Query()
		for key, value := range req.Query {
			query.Set(key, value)
		}

		rurl.RawQuery = query.Encode()
	}

	if err := timeseries.chunk(rurl); err != nil {
		return nil, fmt.Errorf("failed to set time series chunks: %w", err)
	}

	for _, chunk := range timeseries.chunks {
		// copy the request and update it to reflect the partitioned timeseries
		chunkReq := req
		chunkReq.Query[timeseries.StartName] = chunk[0].Format(*timeseries.Layout)
		chunkReq.Query[timeseries.EndName] = chunk[1].Format(*timeseries.Layout)

		fetchConfig := chunkReq.newFetchConfig(rurl, client)

		requests = append(requests, &flattenedRequest{
			fetchConfig: fetchConfig,
			table:       req.Table,
		})
	}

	return requests, nil
}

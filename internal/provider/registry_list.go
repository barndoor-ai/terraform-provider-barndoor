// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// registryListLimit is the page size requested from the registry list
// endpoints (their maximum), so a lookup makes as few round trips as possible.
const registryListLimit = 100

// registryListMaxPages bounds how many pages a lookup will walk before giving
// up. A search-narrowed lookup finds its match in the first page in practice;
// the bound only guards against a misbehaving pagination envelope looping
// forever.
const registryListMaxPages = 100

// registryPage mirrors the registry API's pagination envelope
// (`{"data": [...], "pagination": {...}}`).
type registryPage[T any] struct {
	Data       []T `json:"data"`
	Pagination struct {
		Page     int  `json:"page"`
		Limit    int  `json:"limit"`
		Total    int  `json:"total"`
		Pages    int  `json:"pages"`
		NextPage *int `json:"next_page"`
	} `json:"pagination"`
}

// searchRegistry pages through a registry list endpoint at basePath (query
// carries the endpoint's filter parameters; page/limit are managed here) and
// returns every row keep accepts.
func searchRegistry[T any](ctx context.Context, c *client.Client, basePath string, query url.Values, keep func(T) bool) ([]T, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("limit", strconv.Itoa(registryListLimit))

	var matches []T
	for page := 1; page <= registryListMaxPages; page++ {
		query.Set("page", strconv.Itoa(page))

		var out registryPage[T]
		if err := doJSON(ctx, c, http.MethodGet, basePath+"?"+query.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, item := range out.Data {
			if keep(item) {
				matches = append(matches, item)
			}
		}

		next := out.Pagination.NextPage
		if next == nil || *next <= page || len(out.Data) == 0 {
			return matches, nil
		}
	}
	return nil, fmt.Errorf("GET %s: pagination did not terminate after %d pages", basePath, registryListMaxPages)
}

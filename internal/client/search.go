package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Search finds text in chains visible to this caller. A zero limit uses 20.
func (r *Relay) Search(ctx context.Context, query string, limit int) ([]envelope.SearchResult, error) {
	if limit == 0 {
		limit = 20
	}
	var out struct {
		Results []envelope.SearchResult `json:"results"`
	}
	err := r.Raw(ctx, "GET", "/v1/search?"+url.Values{"q": {query}, "limit": {strconv.Itoa(limit)}}.Encode(), nil, &out)
	if IsStatus(err, http.StatusNotFound) {
		return nil, errors.New("this relay does not support search (upgrade the relay)")
	}
	return out.Results, err
}

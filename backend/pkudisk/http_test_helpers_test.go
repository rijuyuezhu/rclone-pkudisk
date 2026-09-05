package pkudisk

import (
	"context"
	"testing"
)

func mustNewAPIClient(t *testing.T, ctx context.Context, baseURL string, tokens tokenProvider) *apiClient {
	t.Helper()
	client, err := newAPIClient(ctx, baseURL, tokens)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

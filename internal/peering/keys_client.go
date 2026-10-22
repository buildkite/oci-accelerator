package peering

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

var errKeysFetch = errors.New("cache keys fetch failed")

// fetchKeys delivers each validated page to apply in listing order. Earlier
// pages remain applied on later failure. after is both the starting cursor and
// the last validated nonterminal cursor, retained even when the listing ends.
// The caller bounds the fetch with ctx. Every page must identify expected, so
// reused addresses never attribute another node's keys to it. Redirects are
// never followed. An explicitly rejected cursor returns cache.ErrKeyCursor.
func fetchKeys(ctx context.Context, client *http.Client, destination string, expected nodeID, after *cache.KeyPosition, apply func([]cacheKey)) error {
	if client == nil || !validNodeID(expected.String()) {
		return errKeysFetch
	}
	destination, err := CanonicalAddress(destination)
	if err != nil {
		return errKeysFetch
	}
	for {
		page, err := fetchKeysPage(ctx, client, destination, *after)
		if err != nil {
			return err
		}
		if page.node != expected {
			return errKeysFetch
		}
		// Continuations must stay in one store lifetime and advance their
		// logical positions. Individual keys may repeat after live accesses.
		if *after != (cache.KeyPosition{}) && page.next != (cache.KeyPosition{}) {
			if page.next.Incarnation != after.Incarnation || page.next.Last < after.Last || page.next.Last == after.Last && page.next.Key <= after.Key {
				return errKeysFetch
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		apply(page.keys)
		if page.next == (cache.KeyPosition{}) {
			return nil
		}
		*after = page.next
	}
}

// fetchKeysPage requests the page after the cursor, or the first page when
// after is zero.
func fetchKeysPage(ctx context.Context, client *http.Client, destination string, after cache.KeyPosition) (keysPage, error) {
	url := "https://" + destination + keysPath
	if after != (cache.KeyPosition{}) {
		token, err := encodeKeysCursor(after)
		if err != nil {
			return keysPage{}, errKeysFetch
		}
		url += "?after=" + token
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return keysPage{}, errKeysFetch
	}
	request.Header.Set("Accept", "application/json")

	redirectSafe := *client
	redirectSafe.CheckRedirect = rejectRedirect
	response, err := redirectSafe.Do(request)
	if err != nil {
		return keysPage{}, errKeysFetch
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusBadRequest && after != (cache.KeyPosition{}) {
		if response.Header.Get(keysErrorHeader) == "invalid_cursor" {
			return keysPage{}, cache.ErrKeyCursor
		}
		// Older peers identify cursor rejection only in the response body.
		body, err := io.ReadAll(io.LimitReader(response.Body, 32))
		if err == nil && string(body) == "invalid cursor\n" {
			return keysPage{}, cache.ErrKeyCursor
		}
	}
	if response.StatusCode != http.StatusOK || !jsonContentType(response.Header.Get("Content-Type")) {
		return keysPage{}, errKeysFetch
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxKeysPageBytes+1))
	if err != nil || len(encoded) > maxKeysPageBytes {
		return keysPage{}, errKeysFetch
	}
	page, err := decodeKeysPage(encoded)
	if err != nil {
		return keysPage{}, errKeysFetch
	}
	return page, nil
}

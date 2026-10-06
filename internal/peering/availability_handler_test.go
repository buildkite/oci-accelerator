package peering

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAvailabilityHandlerAcceptsValidBatch(t *testing.T) {
	self := mustNodeID(t, testIDC)
	index := newHintIndex(self)
	handler := newAvailabilityHandler(self, mergeInto(index))
	body := availabilityBody(testIDB, testIDC, claimFor(1), claimFor(2))
	response := serveAvailability(handler, http.MethodPost, availabilityPath, "application/json; charset=utf-8", body)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body)
	}
	if index.len() != 2 {
		t.Fatalf("retained %d hints", index.len())
	}
	h := index.find(mustNodeID(t, testIDA), testCacheKey(1))
	if h == nil || !containsNodeID(h.seen, self) {
		t.Fatal("accepted hint is missing or lacks the receiver in seen")
	}
}

func TestAvailabilityHandlerRejectsWrongRecipientWithoutMutation(t *testing.T) {
	self := mustNodeID(t, testIDC)
	index := newHintIndex(self)
	handler := newAvailabilityHandler(self, mergeInto(index))
	response := serveAvailability(handler, http.MethodPost, availabilityPath, "application/json", availabilityBody(testIDB, testNodeID(4), claimFor(1)))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d", response.Code)
	}
	if index.len() != 0 {
		t.Fatal("wrong-recipient request mutated the index")
	}
}

func TestAvailabilityHandlerRejectsInvalidRequestsWithoutMutation(t *testing.T) {
	self := mustNodeID(t, testIDC)
	index := newHintIndex(self)
	handler := newAvailabilityHandler(self, mergeInto(index))
	valid := availabilityBody(testIDB, testIDC, claimFor(1))
	badClaim := `{"ownerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123"]}`
	cases := []struct {
		name, method, target, contentType, body string
	}{
		{"wrong method", http.MethodPut, availabilityPath, "application/json", valid},
		{"wrong path", http.MethodPost, "/v1/cache/availability/", "application/json", valid},
		{"query string", http.MethodPost, availabilityPath + "?x=1", "application/json", valid},
		{"wrong content type", http.MethodPost, availabilityPath, "text/plain", valid},
		{"sender is self", http.MethodPost, availabilityPath, "application/json", availabilityBody(testIDC, testIDB, claimFor(1))},
		{"one invalid claim in a batch", http.MethodPost, availabilityPath, "application/json", availabilityBody(testIDB, testIDC, claimFor(1), badClaim)},
		{"oversized body", http.MethodPost, availabilityPath, "application/json", valid + strings.Repeat(" ", maxAvailabilityBytes)},
	}
	for _, tc := range cases {
		response := serveAvailability(handler, tc.method, tc.target, tc.contentType, tc.body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", tc.name, response.Code)
		}
	}
	if index.len() != 0 {
		t.Fatalf("invalid requests retained %d hints", index.len())
	}
}

func TestAvailabilityHandlerReportsUnavailableAcceptance(t *testing.T) {
	handler := newAvailabilityHandler(mustNodeID(t, testIDC), func(availabilityMessage) error { return errors.New("stopping") })
	response := serveAvailability(handler, http.MethodPost, availabilityPath, "application/json", availabilityBody(testIDB, testIDC, claimFor(1)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func mergeInto(index *hintIndex) func(availabilityMessage) error {
	return func(message availabilityMessage) error {
		for _, c := range message.claims {
			index.merge(c, time.Time{})
		}
		return nil
	}
}

func availabilityBody(sender, recipient string, claims ...string) string {
	return `{"senderNodeId":"` + sender + `","recipientNodeId":"` + recipient + `","claims":[` + strings.Join(claims, ",") + `]}`
}

func serveAvailability(handler http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

package peering

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnnounceHandlerAcceptsValidBatch(t *testing.T) {
	self := mustNodeID(t, testIDC)
	index := newHintIndex(self)
	handler := newAnnounceHandler(self, mergeInto(index))
	body := announcementBody(testIDB, testIDC, hintFor(1), hintFor(2))
	response := serveAnnouncement(handler, http.MethodPost, announcePath, "application/json; charset=utf-8", body)
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

func TestAnnounceHandlerRejectsWrongRecipientWithoutMutation(t *testing.T) {
	self := mustNodeID(t, testIDC)
	index := newHintIndex(self)
	handler := newAnnounceHandler(self, mergeInto(index))
	response := serveAnnouncement(handler, http.MethodPost, announcePath, "application/json", announcementBody(testIDB, testNodeID(4), hintFor(1)))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d", response.Code)
	}
	if index.len() != 0 {
		t.Fatal("wrong-recipient request mutated the index")
	}
}

func TestAnnounceHandlerRejectsInvalidRequestsWithoutMutation(t *testing.T) {
	self := mustNodeID(t, testIDC)
	index := newHintIndex(self)
	handler := newAnnounceHandler(self, mergeInto(index))
	valid := announcementBody(testIDB, testIDC, hintFor(1))
	badHint := `{"providerNodeId":"nodeA-ID_123","key":"` + testKeyText + `","seen":["nodeA-ID_123"]}`
	cases := []struct {
		name, method, target, contentType, body string
	}{
		{"wrong method", http.MethodPut, announcePath, "application/json", valid},
		{"wrong path", http.MethodPost, "/v1/cache/announce/", "application/json", valid},
		{"query string", http.MethodPost, announcePath + "?x=1", "application/json", valid},
		{"wrong content type", http.MethodPost, announcePath, "text/plain", valid},
		{"sender is self", http.MethodPost, announcePath, "application/json", announcementBody(testIDC, testIDB, hintFor(1))},
		{"one invalid hint in a batch", http.MethodPost, announcePath, "application/json", announcementBody(testIDB, testIDC, hintFor(1), badHint)},
		{"oversized body", http.MethodPost, announcePath, "application/json", valid + strings.Repeat(" ", maxAnnouncementBytes)},
	}
	for _, tc := range cases {
		response := serveAnnouncement(handler, tc.method, tc.target, tc.contentType, tc.body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", tc.name, response.Code)
		}
	}
	if index.len() != 0 {
		t.Fatalf("invalid requests retained %d hints", index.len())
	}
}

func TestAnnounceHandlerReportsUnavailableAcceptance(t *testing.T) {
	handler := newAnnounceHandler(mustNodeID(t, testIDC), func(announcement) error { return errors.New("stopping") })
	response := serveAnnouncement(handler, http.MethodPost, announcePath, "application/json", announcementBody(testIDB, testIDC, hintFor(1)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func mergeInto(index *hintIndex) func(announcement) error {
	return func(message announcement) error {
		for _, c := range message.hints {
			index.merge(c, time.Time{})
		}
		return nil
	}
}

func announcementBody(sender, recipient string, hints ...string) string {
	return `{"senderNodeId":"` + sender + `","recipientNodeId":"` + recipient + `","hints":[` + strings.Join(hints, ",") + `]}`
}

func serveAnnouncement(handler http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

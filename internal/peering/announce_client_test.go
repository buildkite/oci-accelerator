package peering

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSendAnnouncementDeliversOverTLS(t *testing.T) {
	self := mustNodeID(t, testIDC)
	var received []announcement
	server := httptest.NewTLSServer(newAnnounceHandler(self, func(message announcement) error {
		received = append(received, message)
		return nil
	}))
	defer server.Close()
	message := announcement{
		sender:    mustNodeID(t, testIDB),
		recipient: self,
		hints:     []announcedHint{testHint(t, testIDA, testCacheKey(1), testIDA, testIDB)},
	}
	if err := sendAnnouncement(context.Background(), server.Client(), serverAddress(server), message); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || !reflect.DeepEqual(received[0], message) {
		t.Fatalf("received %+v", received)
	}
}

func TestSendAnnouncementDeliversLargestBatch(t *testing.T) {
	self := mustNodeID(t, testIDC)
	index := newHintIndex(self)
	server := httptest.NewTLSServer(newAnnounceHandler(self, mergeInto(index)))
	defer server.Close()
	ids := []string{testIDA, testIDB}
	for i := range maxHintSeen - 2 {
		ids = append(ids, testNodeID(i))
	}
	// 33 hints with full seen sets fit the byte bound, and 34 do not.
	message := announcement{sender: mustNodeID(t, testIDB), recipient: self}
	for i := range 34 {
		message.hints = append(message.hints, testHint(t, testIDA, testCacheKey(i), ids...))
	}
	if err := sendAnnouncement(context.Background(), server.Client(), serverAddress(server), message); err == nil {
		t.Fatal("oversized batch sent")
	}
	message.hints = message.hints[:33]
	if err := sendAnnouncement(context.Background(), server.Client(), serverAddress(server), message); err != nil {
		t.Fatal(err)
	}
	if index.len() != 33 {
		t.Fatalf("retained %d hints", index.len())
	}
}

func TestSendAnnouncementRequiresNoContent(t *testing.T) {
	message := announcement{
		sender:    mustNodeID(t, testIDB),
		recipient: mustNodeID(t, testIDC),
		hints:     []announcedHint{testHint(t, testIDA, testCacheKey(1), testIDA, testIDB)},
	}
	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		if err := sendAnnouncement(context.Background(), server.Client(), serverAddress(server), message); err == nil {
			t.Errorf("status %d counted as an acknowledgement", status)
		}
		server.Close()
	}
}

func TestSendAnnouncementDoesNotFollowRedirects(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+announcePath, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := redirect.Client()
	client.CheckRedirect = nil // the sender must not rely on the caller's policy
	message := announcement{
		sender:    mustNodeID(t, testIDB),
		recipient: mustNodeID(t, testIDC),
		hints:     []announcedHint{testHint(t, testIDA, testCacheKey(1), testIDA, testIDB)},
	}
	if err := sendAnnouncement(context.Background(), client, serverAddress(redirect), message); err == nil {
		t.Fatal("redirect counted as an acknowledgement")
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d", targetCalls.Load())
	}
}

func TestSendAnnouncementRejectsInvalidBatchBeforeSending(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender, recipient := mustNodeID(t, testIDB), mustNodeID(t, testIDC)
	invalid := []announcement{
		{sender: sender, recipient: recipient},
		{sender: sender, recipient: recipient, hints: []announcedHint{testHint(t, testIDA, testCacheKey(1), testIDA)}},
	}
	for _, message := range invalid {
		if err := sendAnnouncement(context.Background(), server.Client(), serverAddress(server), message); err == nil {
			t.Errorf("invalid batch %+v sent", message)
		}
	}
	valid := announcement{sender: sender, recipient: recipient, hints: []announcedHint{testHint(t, testIDA, testCacheKey(1), testIDA, testIDB)}}
	if err := sendAnnouncement(context.Background(), server.Client(), "bad address", valid); err == nil {
		t.Error("invalid destination accepted")
	}
	if calls.Load() != 0 {
		t.Fatalf("server calls = %d", calls.Load())
	}
}

func serverAddress(server *httptest.Server) string {
	return strings.TrimPrefix(server.URL, "https://")
}

package peering

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

const (
	peersPath       = "/v1/peers"
	maxMessageBytes = 512 << 10
)

var errExchange = errors.New("peer exchange failed")

// messageFields lists the exact top-level keys of a wire message. Exact
// matching stops encoding/json's case-insensitive field matching from
// accepting two spellings of one field.
var messageFields = map[string]bool{"address": true, "nodeId": true, "peers": true}

// NewHandler serves read-only GET snapshots and POST exchanges at /v1/peers.
func NewHandler(snapshot func() Message, exchange func(Message) (Message, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != peersPath {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		var response Message
		switch r.Method {
		case http.MethodGet:
			response = snapshot()
		case http.MethodPost:
			if !jsonContentType(r.Header.Get("Content-Type")) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}

			encoded, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessageBytes))
			if err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}

			request, err := decodeMessage(encoded)
			if err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}

			response, err = exchange(request)
			if err != nil {
				http.Error(w, "exchange failed", http.StatusInternalServerError)
				return
			}
		default:
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		encoded, err := encodeMessage(response)
		if err != nil {
			http.Error(w, "exchange failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	})
}

// encodeMessage canonicalizes and bounds a message for the wire.
func encodeMessage(message Message) ([]byte, error) {
	message, err := canonicalMessage(message)
	if err != nil {
		return nil, errInvalidMessage
	}

	encoded, err := json.Marshal(message)
	if err != nil || len(encoded) > maxMessageBytes {
		return nil, errInvalidMessage
	}
	return encoded, nil
}

// decodeMessage strictly decodes and canonicalizes one wire message. It
// rejects duplicate object keys, which could otherwise hide contradictory
// peer records, as well as unknown fields and trailing data.
func decodeMessage(encoded []byte) (Message, error) {
	if len(encoded) > maxMessageBytes || checkMessageKeys(encoded) != nil {
		return Message{}, errInvalidMessage
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var message Message
	if decoder.Decode(&message) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Message{}, errInvalidMessage
	}
	return canonicalMessage(message)
}

// checkMessageKeys requires exactly one top-level JSON object with every
// field present under its exact name, a flat peers object, and no duplicate
// keys. Values that cannot fit the Message shape are rejected here without
// recursing into them.
func checkMessageKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	fields := 0
	err := checkObjectKeys(decoder, messageFields, func(key string) error {
		fields++
		if key != "peers" {
			return checkString(decoder)
		}
		return checkObjectKeys(decoder, nil, func(string) error { return checkString(decoder) })
	})
	if err != nil || fields != len(messageFields) {
		return errInvalidMessage
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidMessage
	}
	return nil
}

// checkObjectKeys consumes one object, rejecting duplicate keys and, when
// allowed is non-nil, keys outside allowed. value consumes each member value.
func checkObjectKeys(decoder *json.Decoder, allowed map[string]bool, value func(key string) error) error {
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errInvalidMessage
	}

	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return errInvalidMessage
		}
		if _, duplicate := seen[key]; duplicate || allowed != nil && !allowed[key] {
			return errInvalidMessage
		}
		seen[key] = struct{}{}
		if err := value(key); err != nil {
			return err
		}
	}

	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return errInvalidMessage
	}
	return nil
}

// checkString consumes a string value, rejecting null and structured values.
func checkString(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if _, ok := token.(string); err != nil || !ok {
		return errInvalidMessage
	}
	return nil
}

func jsonContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

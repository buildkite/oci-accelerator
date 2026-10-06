package peering

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

const testDigestText = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func TestDecodeFetchRequestAcceptsValidRequest(t *testing.T) {
	want := fetchRequest{RecipientNodeID: testIDA, Key: testKeyText, Digest: testDigestText, Size: 42, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFetchRequest(encoded)
	if err != nil || got != want {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	meta := got.metadata()
	if meta != (cache.Metadata{Key: testKeyText, Digest: testDigestText, Size: 42, MediaType: want.MediaType}) {
		t.Fatalf("metadata = %+v", meta)
	}
}

func TestDecodeFetchRequestFitsEscapedMaximumMediaType(t *testing.T) {
	prefix := `application/x; p="`
	mediaType := prefix + strings.Repeat("<", maxFetchMediaType-len(prefix)-1) + `"`
	want := fetchRequest{RecipientNodeID: testIDA, Key: testKeyText, Digest: testDigestText, Size: 1<<63 - 1, MediaType: mediaType}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= 4*maxFetchMediaType {
		t.Fatalf("encoding is only %d bytes, so the media type was not escaped", len(encoded))
	}
	got, err := decodeFetchRequest(encoded)
	if err != nil || got != want {
		t.Fatalf("decoded %+v, %v", got, err)
	}
}

func TestDecodeFetchRequestRejectsInvalidRequests(t *testing.T) {
	field := func(name, value string) string {
		values := map[string]string{
			"recipientNodeId": `"` + testIDA + `"`,
			"key":             `"` + testKeyText + `"`,
			"digest":          `"` + testDigestText + `"`,
			"size":            "42",
			"mediaType":       `"application/octet-stream"`,
		}
		values[name] = value
		var fields []string
		for _, key := range []string{"recipientNodeId", "key", "digest", "size", "mediaType"} {
			if values[key] != "" {
				fields = append(fields, `"`+key+`":`+values[key])
			}
		}
		return "{" + strings.Join(fields, ",") + "}"
	}
	valid := field("", "")
	if _, err := decodeFetchRequest([]byte(valid)); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	cases := []struct{ name, body string }{
		{"empty", ""},
		{"array", "[" + valid + "]"},
		{"null", "null"},
		{"trailing data", valid + "{}"},
		{"oversized", valid + strings.Repeat(" ", maxFetchRequestBytes)},
		{"unknown field", strings.TrimSuffix(valid, "}") + `,"extra":"x"}`},
		{"duplicate field", strings.TrimSuffix(valid, "}") + `,"key":"` + testKeyText + `"}`},
		{"other spelling", strings.Replace(valid, `"key"`, `"Key"`, 1)},
		{"missing recipient", field("recipientNodeId", "")},
		{"missing key", field("key", "")},
		{"missing digest", field("digest", "")},
		{"missing size", field("size", "")},
		{"missing media type", field("mediaType", "")},
		{"null recipient", field("recipientNodeId", "null")},
		{"null size", field("size", "null")},
		{"null media type", field("mediaType", "null")},
		{"structured key", field("key", `{"a":"b"}`)},
		{"short recipient", field("recipientNodeId", `"nodeA"`)},
		{"invalid recipient", field("recipientNodeId", `"nodeA-ID_12!"`)},
		{"uppercase key", field("key", `"`+strings.ToUpper(testKeyText)+`"`)},
		{"short key", field("key", `"`+testKeyText[1:]+`"`)},
		{"digest without algorithm", field("digest", `"`+testKeyText+`"`)},
		{"other digest algorithm", field("digest", `"sha512:`+testKeyText+`"`)},
		{"uppercase digest", field("digest", `"sha256:`+strings.ToUpper(testKeyText)+`"`)},
		{"negative size", field("size", "-1")},
		{"negative zero size", field("size", "-0")},
		{"fractional size", field("size", "1.0")},
		{"exponent size", field("size", "1e3")},
		{"string size", field("size", `"42"`)},
		{"overflowing size", field("size", "9223372036854775808")},
		{"empty media type", field("mediaType", `""`)},
		{"unparseable media type", field("mediaType", `"application/"`)},
		{"media type with CR", field("mediaType", `"application/json; a=\"x\ry\""`)},
		{"media type with LF", field("mediaType", `"application/json; a=\"x\ny\""`)},
		{"overlong media type", field("mediaType", `"application/x; p=`+strings.Repeat("a", maxFetchMediaType)+`"`)},
	}
	for _, tc := range cases {
		if got, err := decodeFetchRequest([]byte(tc.body)); err == nil {
			t.Errorf("%s: accepted %+v", tc.name, got)
		}
	}
}

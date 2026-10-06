package peering

import "fmt"

const testSelf = "self.test:1"

// claimFields is Claim without Seen, so claims can be compared with ==.
type claimFields struct {
	Owner, Session, Key string
	Revision            uint64
	Present             bool
	Expires             int64
}

func fieldsOf(c Claim) claimFields {
	return claimFields{c.Owner, c.Session, c.Key, c.Revision, c.Present, c.Expires}
}

func testKey(i int) string { return fmt.Sprintf("%064x", i) }

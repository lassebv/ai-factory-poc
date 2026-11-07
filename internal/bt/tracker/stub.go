package tracker

import (
	"math/rand/v2"
	"net/http"
)

// STUB: placeholders so the tree compiles before the implementation exists.
// The implementer deletes this file and provides real versions (see the
// list in contract.go).

const errStub = "tracker: not implemented"

func newHTTPClient(*http.Client) Client                     { panic(errStub) }
func newAnnouncer([][]string, Client, *rand.Rand) Announcer { panic(errStub) }
func startLoop(LoopConfig) Loop                             { panic(errStub) }

// String returns the event name used in the announce query.
func (e Event) String() string { panic(errStub) }

// Error returns the failure reason.
func (e *FailureError) Error() string { panic(errStub) }

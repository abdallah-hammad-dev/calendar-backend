// Package adapter is the one interface behind which each kind of external account lives (spec §4).
package adapter

import (
	"context"
	"errors"

	"github.com/emersion/go-ical"
)

// ErrReadOnly: the account can't carry an answer back (an ICS feed). The answer stays on Davis only.
var ErrReadOnly = errors.New("this account is read-only: the answer was not sent")

// Adapter reads one external account's calendar and answers its invitations.
type Adapter interface {
	// List returns what changed since token ("" = everything). An invalid or expired token is the adapter's to
	// handle: it lists everything and says so with Page.Full.
	List(ctx context.Context, token string) (Page, error)
	// RSVP answers the invitation sourceID with partstat (ACCEPTED, TENTATIVE, DECLINED) from the invited address.
	RSVP(ctx context.Context, sourceID, partstat string) error
}

type Page struct {
	Changes []Change
	Token   string // to pass to the next List
	Full    bool   // everything is listed: events of the account missing from Changes are gone
}

// Change is one event of the account, master and overrides together, as the account has it.
type Change struct {
	UID      string
	SourceID string // the provider's id for it, given back to RSVP
	Deleted  bool
	// Calendar holds the event's VEVENTs (and the VTIMEZONEs they use, when the source has them), already in
	// iCalendar terms (spec §5.2): summary, times, recurrence, location, plain-text description, organizer,
	// attendees, status, CONFERENCE. The mirror then adds what is the same for every account (my attendee
	// address, X-SOURCE-*, URL, VTIMEZONE). Nil when Deleted.
	Calendar *ical.Calendar
	// Me is the address that was invited in this event, if not the account's own (an alias, a group): "" = the
	// account's address.
	Me string
}

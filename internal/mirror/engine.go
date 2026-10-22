package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

// Engine runs the passes: each account's source into the collection I chose for it, and those collections back
// (my answers). Passes run one after another, never concurrently.
//
// A collection is mine: it also holds my own events and tasks, maybe other accounts' events. The engine only
// touches the items it wrote (the store knows their hrefs) and leaves everything else alone.
//
// Logs carry UIDs and hrefs, never event contents.
type Engine struct {
	Store   *store.Store
	Davis   *davis.Client
	Me      string // my address on Davis
	Home    string // my calendar home on Davis, where the collections are
	Vault   string
	Loc     *time.Location
	Adapter func(store.Account) (adapter.Adapter, error)
	Log     *slog.Logger
	Now     func() time.Time // nil = time.Now

	emptySeen map[int64]bool // accounts whose last full listing was empty while we held events
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) target(a store.Account) Target {
	return Target{Account: a.Kind + ":" + ics.Address(a.Address), Me: e.Me, Vault: e.Vault, Loc: e.Loc}
}

// ErrCollection: the collection asked for doesn't exist, is ambiguous, or can't hold events.
var ErrCollection = errors.New("collection")

// Collection finds one of my collections by href or by name (case aside). It must hold events.
func (e *Engine) Collection(ctx context.Context, nameOrHref string) (davis.Calendar, error) {
	cals, err := e.Davis.Calendars(ctx, e.Home)
	if err != nil {
		return davis.Calendar{}, err
	}
	var found []davis.Calendar
	var names []string
	for _, c := range cals {
		names = append(names, fmt.Sprintf("%q", c.DisplayName))
		if davis.SamePath(c.Href, nameOrHref) || strings.EqualFold(c.DisplayName, nameOrHref) {
			found = append(found, c)
		}
	}
	switch {
	case len(found) == 0:
		return davis.Calendar{}, fmt.Errorf("%w %q not found; mine: %s", ErrCollection, nameOrHref, strings.Join(names, ", "))
	case len(found) > 1:
		return davis.Calendar{}, fmt.Errorf("%w: several are named %q, give its href", ErrCollection, nameOrHref)
	}
	c := found[0]
	for _, comp := range c.Components {
		if comp == ical.CompEvent {
			return c, nil
		}
	}
	return davis.Calendar{}, fmt.Errorf("%w %q holds no events (only %s)", ErrCollection, c.DisplayName, strings.Join(c.Components, ", "))
}

// AddAccount records an account that writes into one of my collections (by name or href). The backend makes no
// collection: a collection only for this account is one I made for it.
func (e *Engine) AddAccount(ctx context.Context, a store.Account, collection string) (store.Account, error) {
	c, err := e.Collection(ctx, collection)
	if err != nil {
		return a, err
	}
	a.Collection = c.Href
	if a.ID, err = e.Store.AddAccount(ctx, a); err != nil {
		return a, err
	}
	e.Log.Info("account added", "account", a.ID, "kind", a.Kind, "collection", a.Collection)
	return a, nil
}

// RemoveAccount forgets the account. Its events are deleted from Davis when asked to; otherwise they stay as they
// are (still marked as mirrored, so read-only in the extension, and no longer updated).
func (e *Engine) RemoveAccount(ctx context.Context, a store.Account, deleteEvents bool) error {
	if deleteEvents {
		items, err := e.Store.Items(ctx, a.ID)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.Href == "" {
				continue
			}
			if err := e.Davis.Delete(ctx, it.Href, ""); err != nil && !errors.Is(err, davis.ErrNotFound) {
				return err
			}
		}
	}
	return e.Store.DeleteAccount(ctx, a.ID)
}

// Pass reads each collection that accounts write into (answers made since the last pass must be seen before a
// source can overwrite them), then, when withSource, each account's source.
func (e *Engine) Pass(ctx context.Context, withSource bool) error {
	accounts, err := e.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	byCollection := map[string][]store.Account{}
	var order []string
	for _, a := range accounts {
		if _, ok := byCollection[a.Collection]; !ok {
			order = append(order, a.Collection)
		}
		byCollection[a.Collection] = append(byCollection[a.Collection], a)
	}
	var errs []error
	for _, coll := range order {
		werr := e.Watch(ctx, coll, byCollection[coll])
		if werr != nil {
			e.Log.Warn("collection read failed", "collection", coll, "err", werr)
		}
		for _, a := range byCollection[coll] {
			err := werr
			if err == nil && withSource {
				err = e.Pull(ctx, a)
			}
			if err != nil {
				e.Log.Warn("pass failed", "account", a.ID, "err", err)
				errs = append(errs, fmt.Errorf("account %d: %w", a.ID, err))
			}
			if serr := e.Store.SetSyncResult(ctx, a.ID, err); serr != nil {
				errs = append(errs, serr)
			}
		}
	}
	return errors.Join(errs...)
}

// Run passes every davisEvery, with the sources every sourceEvery, until ctx ends.
func (e *Engine) Run(ctx context.Context, davisEvery, sourceEvery time.Duration) error {
	var lastSource time.Time
	t := time.NewTicker(davisEvery)
	defer t.Stop()
	for {
		withSource := e.now().Sub(lastSource) >= sourceEvery
		if withSource {
			lastSource = e.now()
		}
		e.Pass(ctx, withSource) // failures are logged and kept per account; the next pass tries again
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Pull copies what changed on the account into its collection.
func (e *Engine) Pull(ctx context.Context, a store.Account) error {
	ad, err := e.Adapter(a)
	if err != nil {
		return err
	}
	page, err := ad.List(ctx, a.SourceToken)
	if err != nil {
		return err
	}
	var errs []error
	seen := map[string]bool{}
	for _, ch := range page.Changes {
		seen[ch.UID] = true
		if ch.Deleted {
			err = e.remove(ctx, a, ch.UID)
		} else {
			err = e.upsert(ctx, a, ch)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ch.UID, err))
		}
	}
	if page.Full {
		items, err := e.Store.Items(ctx, a.ID)
		if err != nil {
			return err
		}
		// An empty listing is believed only the second time in a row (as in the extension): one bad answer from
		// a provider must not delete every mirrored event.
		if len(page.Changes) == 0 && len(items) > 0 && !e.emptySeen[a.ID] {
			if e.emptySeen == nil {
				e.emptySeen = map[int64]bool{}
			}
			e.emptySeen[a.ID] = true
			e.Log.Warn("empty listing, believed if it comes again", "account", a.ID)
			return nil
		}
		delete(e.emptySeen, a.ID)
		for _, it := range items {
			if !seen[it.UID] {
				if err := e.remove(ctx, a, it.UID); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", it.UID, err))
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...) // the token stays: the same changes come again next time
	}
	return e.Store.SetSourceToken(ctx, a.ID, page.Token)
}

func (e *Engine) upsert(ctx context.Context, a store.Account, ch adapter.Change) error {
	it, err := e.Store.Item(ctx, a.ID, ch.UID)
	if errors.Is(err, store.ErrNotFound) {
		it = store.Item{AccountID: a.ID, UID: ch.UID}
	} else if err != nil {
		return err
	}
	it.SourceID = ch.SourceID
	it.Invited = ch.Me
	it.SourceData = ics.Encode(ch.Calendar)
	if it.Answer != "" && ics.MyPartstat(ch.Calendar, e.invited(a, it)) == it.Answer {
		e.Log.Info("answer confirmed by the source", "account", a.ID, "uid", it.UID, "partstat", it.Answer)
		it.Answer = ""
	}
	if it.Href == "" {
		// Invited on two addresses whose accounts write into the same collection: one copy (Davis takes two items
		// with the same UID, and shows both). The first account keeps it; this one waits, recorded unwritten
		// (no base hash), and is written if that copy goes (handOver).
		taken, err := e.copiedByOther(ctx, a, it.UID)
		if err != nil {
			return err
		}
		if taken {
			it.BaseHash = ""
			return e.Store.PutItem(ctx, it)
		}
	}
	return e.write(ctx, a, &it, nil)
}

// copiedByOther tells whether another account writing into a's collection has its copy of uid there, or had one
// that I deleted on Davis (not to be brought back through this account either).
func (e *Engine) copiedByOther(ctx context.Context, a store.Account, uid string) (bool, error) {
	others, err := e.Store.ItemsByUID(ctx, uid)
	if err != nil {
		return false, err
	}
	for _, o := range others {
		if o.AccountID == a.ID || o.BaseHash == "" { // itself, or another one waiting
			continue
		}
		oa, err := e.Store.Account(ctx, o.AccountID)
		if err != nil {
			return false, err
		}
		if oa.Collection == a.Collection {
			return true, nil
		}
	}
	return false, nil
}

// handOver writes the copy of uid an account was holding back (see upsert) once the copy it deferred to is gone.
func (e *Engine) handOver(ctx context.Context, collection, uid string) error {
	waiting, err := e.Store.ItemsByUID(ctx, uid)
	if err != nil {
		return err
	}
	for _, it := range waiting {
		if it.Href != "" || it.BaseHash != "" {
			continue // written, or deleted by me on Davis (not brought back)
		}
		a, err := e.Store.Account(ctx, it.AccountID)
		if err != nil || a.Collection != collection {
			continue
		}
		e.Log.Info("copy handed over", "account", a.ID, "uid", uid)
		return e.write(ctx, a, &it, nil)
	}
	return nil
}

func (e *Engine) invited(a store.Account, it store.Item) string {
	if it.Invited != "" {
		return it.Invited
	}
	return a.Address
}

// write brings the Davis copy of it in line with its source: built from the source, my answer kept, merged with
// what clients added to the copy. cur is the Davis copy when the caller already has it.
//
// Nothing is written when the source gave nothing new and the copy wasn't changed on Davis since the last write
// (the Davis watch passes changed copies in as cur). An event deleted on Davis stays deleted until the source
// changes it (spec §11 is open on what deleting a mirrored invitation should mean).
func (e *Engine) write(ctx context.Context, a store.Account, it *store.Item, cur *davis.Item) error {
	if cur == nil {
		_, hash, err := e.base(a, it)
		if err != nil {
			return err
		}
		if hash == it.BaseHash {
			return e.Store.PutItem(ctx, *it)
		}
		if it.Href != "" {
			got, err := e.Davis.Get(ctx, it.Href)
			if err == nil {
				cur = &got
			} else if !errors.Is(err, davis.ErrNotFound) {
				return err
			}
		}
	}
	if cur != nil && cur.ETag != it.ETag {
		e.observe(a, it, *cur) // my answer may have changed since the last write: it goes into the build
	}

	for attempt := 0; ; attempt++ {
		base, hash, err := e.base(a, it)
		if err != nil {
			return err
		}
		var curCal *ical.Calendar
		if cur != nil {
			if curCal, err = ics.Parse(cur.Data); err != nil {
				e.Log.Warn("unreadable copy on Davis, replaced", "account", a.ID, "uid", it.UID, "err", err)
			}
		}
		merged := Merge(base, curCal)
		EnsureStamp(merged, curCal, e.now())
		data := ics.Encode(merged)
		partstat := ics.MyPartstat(merged, e.Me)
		if curCal != nil && ics.Encode(curCal) == data {
			it.ETag, it.BaseHash, it.Partstat = cur.ETag, hash, partstat
			return e.Store.PutItem(ctx, *it)
		}

		href, ifMatch := it.Href, ""
		if href == "" {
			href = a.Collection + FileName(it.UID)
		}
		if cur != nil {
			ifMatch = cur.ETag
		}
		etag, err := e.Davis.Put(ctx, href, data, ifMatch)
		if errors.Is(err, davis.ErrPrecondition) && attempt == 0 {
			// Changed on Davis meanwhile, or already there: a run stopped before recording its write, or an item
			// that isn't this account's (same UID, made by me or another client). Only the first is taken over.
			got, gerr := e.Davis.Get(ctx, href)
			if gerr != nil {
				return gerr
			}
			if it.Href == "" && !e.owns(a, got) {
				e.Log.Warn("an item that isn't this account's is already there, left alone", "account", a.ID, "uid", it.UID, "href", href)
				return nil
			}
			it.Href, cur = href, &got
			e.observe(a, it, got)
			continue
		}
		if err != nil {
			return err
		}
		if etag == "" { // Davis changed what it stored (sabre adds scheduling details): read the stored one's ETag
			got, err := e.Davis.Get(ctx, href)
			if err != nil {
				return err
			}
			etag = got.ETag
		}
		it.Href, it.ETag, it.BaseHash, it.Partstat = href, etag, hash, partstat
		e.Log.Info("mirrored", "account", a.ID, "uid", it.UID, "href", href)
		return e.Store.PutItem(ctx, *it)
	}
}

// base is the copy built from the source alone, and its hash.
func (e *Engine) base(a store.Account, it *store.Item) (*ical.Calendar, string, error) {
	src, err := ics.Parse(it.SourceData)
	if err != nil {
		return nil, "", err
	}
	base := Base(src, e.invited(a, *it), it.Answer, e.target(a))
	return base, Hash(ics.Encode(base)), nil
}

// owns tells whether a Davis item is one of this account's copies (its X-SOURCE-ACCOUNT).
func (e *Engine) owns(a store.Account, item davis.Item) bool {
	cal, err := ics.Parse(item.Data)
	if err != nil {
		return false
	}
	for _, ev := range ics.Events(cal) {
		if ics.Value(ev, PropSourceAccount) == e.target(a).Account {
			return true
		}
	}
	return false
}

// observe reads my answer on a Davis copy that changed since the backend wrote it: a PARTSTAT other than the one
// written is an answer made on the iPhone or in Obsidian (compared alone: iOS rewrites the other parameters).
func (e *Engine) observe(a store.Account, it *store.Item, got davis.Item) {
	cal, err := ics.Parse(got.Data)
	if err != nil {
		return
	}
	p := ics.MyPartstat(cal, e.Me)
	if p != "" && p != it.Partstat && p != it.Answer {
		e.Log.Info("answer made on Davis", "account", a.ID, "uid", it.UID, "partstat", p)
		it.Answer = p
	}
}

func (e *Engine) remove(ctx context.Context, a store.Account, uid string) error {
	it, err := e.Store.Item(ctx, a.ID, uid)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if it.Href != "" {
		// Unconditional: the event is gone from the source, whatever a client changed in its copy.
		if err := e.Davis.Delete(ctx, it.Href, ""); err != nil && !errors.Is(err, davis.ErrNotFound) {
			return err
		}
		e.Log.Info("removed", "account", a.ID, "uid", uid, "href", it.Href)
	}
	if err := e.Store.DeleteItem(ctx, a.ID, uid); err != nil {
		return err
	}
	if it.Href != "" {
		return e.handOver(ctx, a.Collection, uid)
	}
	return nil
}

// Watch reads what changed in a collection that accounts write into since the last pass: my answers on their
// events, and edits a client made that the source's version replaces (mirrored events are read-only except my
// PARTSTAT, spec §5.1). Everything else there (my own events and tasks) is left alone.
func (e *Engine) Watch(ctx context.Context, coll string, accounts []store.Account) error {
	token, err := e.Store.CollectionToken(ctx, coll)
	if err != nil {
		return err
	}
	ch, err := e.Davis.Sync(ctx, coll, token)
	if err != nil {
		return err
	}
	byID := map[int64]store.Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	var errs []error
	listed := map[string]bool{}
	for _, item := range ch.Changed {
		listed[item.Href] = true
		it, err := e.Store.ItemByHref(ctx, item.Href)
		if errors.Is(err, store.ErrNotFound) {
			continue // not a copy the backend made
		} else if err != nil {
			return err
		}
		a, ok := byID[it.AccountID]
		if !ok || item.ETag == it.ETag {
			continue // the backend's own write
		}
		if item.Data == "" {
			if item, err = e.Davis.Get(ctx, item.Href); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		e.observe(a, &it, item)
		if err := e.write(ctx, a, &it, &item); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", it.UID, err))
		}
	}

	removed := ch.Removed
	if ch.Full {
		// A full listing says what's gone by leaving it out. Each is checked, so that one bad answer from the
		// server can't make the backend forget every copy.
		for _, a := range accounts {
			items, err := e.Store.Items(ctx, a.ID)
			if err != nil {
				return err
			}
			for _, it := range items {
				if it.Href == "" || listed[it.Href] || !strings.HasPrefix(it.Href, coll) {
					continue
				}
				if _, err := e.Davis.Get(ctx, it.Href); errors.Is(err, davis.ErrNotFound) {
					removed = append(removed, it.Href)
				}
			}
		}
	}
	for _, href := range removed {
		it, err := e.Store.ItemByHref(ctx, href)
		if errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		e.Log.Info("copy deleted on Davis", "account", it.AccountID, "uid", it.UID, "href", href)
		it.Href, it.ETag = "", ""
		if err := e.Store.PutItem(ctx, it); err != nil {
			return err
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return e.Store.SetCollectionToken(ctx, coll, ch.Token)
}

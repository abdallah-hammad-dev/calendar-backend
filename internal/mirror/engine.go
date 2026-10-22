package mirror

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/emersion/go-ical"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

// Engine runs the passes: each account's source into its mirror collection, and the mirror collections back
// (my answers). Passes run one after another, never concurrently.
//
// Logs carry UIDs and hrefs, never event contents.
type Engine struct {
	Store   *store.Store
	Davis   *davis.Client
	Me      string // my address on Davis
	Home    string // my calendar home on Davis, where mirror collections are made
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

// AddAccount creates the account's mirror collection (events only) and records the account.
func (e *Engine) AddAccount(ctx context.Context, a store.Account) (store.Account, error) {
	var b [4]byte
	rand.Read(b[:])
	a.Collection = e.Home + "mirror-" + hex.EncodeToString(b[:]) + "/"
	if err := e.Davis.MakeCalendar(ctx, a.Collection, a.Name, a.Color, ical.CompEvent); err != nil {
		return a, err
	}
	id, err := e.Store.AddAccount(ctx, a)
	if err != nil {
		e.Davis.Delete(ctx, a.Collection, "")
		return a, err
	}
	a.ID = id
	e.Log.Info("account added", "account", id, "kind", a.Kind, "collection", a.Collection)
	return a, nil
}

// RemoveAccount forgets the account, and deletes its mirror collection when asked to.
func (e *Engine) RemoveAccount(ctx context.Context, a store.Account, deleteCollection bool) error {
	if deleteCollection {
		if err := e.Davis.Delete(ctx, a.Collection, ""); err != nil && !errors.Is(err, davis.ErrNotFound) {
			return err
		}
	}
	return e.Store.DeleteAccount(ctx, a.ID)
}

// Pass runs every account once: the mirror collection first (answers made since the last pass must be seen before
// the source can overwrite them), then the source when withSource.
func (e *Engine) Pass(ctx context.Context, withSource bool) error {
	accounts, err := e.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accounts {
		err := e.Watch(ctx, a)
		if err == nil && withSource {
			var fresh store.Account // Watch moved its token on
			if fresh, err = e.Store.Account(ctx, a.ID); err == nil {
				err = e.Pull(ctx, fresh)
			}
		}
		if err != nil {
			e.Log.Warn("pass failed", "account", a.ID, "err", err)
			errs = append(errs, fmt.Errorf("account %d: %w", a.ID, err))
		}
		if serr := e.Store.SetSyncResult(ctx, a.ID, err); serr != nil {
			errs = append(errs, serr)
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

// Pull copies what changed on the account into its mirror collection.
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
	return e.write(ctx, a, &it, nil)
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
			// Changed on Davis meanwhile, or already there (a run stopped before recording its write): read it,
			// and build again on it.
			got, gerr := e.Davis.Get(ctx, href)
			if gerr != nil {
				return gerr
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
	return e.Store.DeleteItem(ctx, a.ID, uid)
}

// Watch reads what changed in the account's mirror collection since the last pass: my answers, and edits a client
// made that the source's version replaces (mirror events are read-only except my PARTSTAT, spec §5.1).
func (e *Engine) Watch(ctx context.Context, a store.Account) error {
	ch, err := e.Davis.Sync(ctx, a.Collection, a.DavisToken)
	if err != nil {
		return err
	}
	var errs []error
	listed := map[string]bool{}
	for _, item := range ch.Changed {
		listed[item.Href] = true
		it, err := e.Store.ItemByHref(ctx, a.ID, item.Href)
		if errors.Is(err, store.ErrNotFound) {
			// Made by a client in a mirror collection: left alone (accepted only in part 2, spec §9).
			e.Log.Info("event created in a mirror collection, left alone", "account", a.ID, "href", item.Href)
			continue
		} else if err != nil {
			return err
		}
		if item.ETag == it.ETag {
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
		items, err := e.Store.Items(ctx, a.ID)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.Href == "" || listed[it.Href] {
				continue
			}
			if _, err := e.Davis.Get(ctx, it.Href); errors.Is(err, davis.ErrNotFound) {
				removed = append(removed, it.Href)
			}
		}
	}
	for _, href := range removed {
		it, err := e.Store.ItemByHref(ctx, a.ID, href)
		if errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		e.Log.Info("copy deleted on Davis", "account", a.ID, "uid", it.UID, "href", href)
		it.Href, it.ETag = "", ""
		if err := e.Store.PutItem(ctx, it); err != nil {
			return err
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return e.Store.SetDavisToken(ctx, a.ID, ch.Token)
}

// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package application

import "slices"

// Clone returns a Status that shares no memory with this one.
//
// It exists because a Status is handed out by value while carrying slices and
// pointers, so "by value" copies the headers and not what they point at. The
// reconciler holds one per application and answers every read from it, so every
// caller — the list endpoint, the detail endpoint, the app-set loop's diff, and
// whatever the companion adds — was given the same backing array and the same
// *ReleaseDrift, *Compat and *SyncResult. That was safe only because the store
// is replaced wholesale on each reconcile and no consumer happened to mutate:
// an undocumented invariant that the first caller to sort Releases in place,
// or to append to it, would break — and break in the store, for every other
// caller at once, under the reconciler's read lock where nothing would look.
//
// Answering with a snapshot is the contract that removes the invariant rather
// than documenting it. The cost is one shallow copy per release per read, on a
// path that already serialises the result to JSON.
//
// Every reference-typed field has to be copied here, and TestCloneSharesNothing
// WithItsOriginal is what enforces that: it builds a value with every reference
// field populated *by reflection*, clones it, and fails on any pointer or slice
// the two still share. Populating by reflection rather than by hand is the whole
// of what makes it a guard — the first version of this test used a hand-written
// fixture, and Spec.Allow was added the same day and went unchecked, because a
// fixture can only cover the fields somebody remembered to write into it.
func (s Status) Clone() Status {
	out := s
	out.Sync.LastSync = clonePtr(s.Sync.LastSync)
	out.Drift = clonePtr(s.Drift)

	out.Releases = slices.Clone(s.Releases)
	for i := range out.Releases {
		rel := &out.Releases[i]
		rel.Services = slices.Clone(rel.Services)
		rel.Compat = clonePtr(rel.Compat)
		rel.Drift = cloneReleaseDrift(rel.Drift)
	}
	return out
}

// Clone returns a Spec that shares no memory with this one.
//
// Same reason as Status, and the same two callers: a Spec is handed out by value
// but carries *ChartSource — whose Values and Repositories are slices — and
// SyncPolicy's *int. The store's copy is what the app-set loop diffs the next
// desired set against, so a caller writing through any of those would change
// what "unchanged" means for that application on every pass afterwards.
func (s Spec) Clone() Spec {
	out := s
	out.SyncPolicy.HistoryMax = clonePtr(s.SyncPolicy.HistoryMax)

	out.Allow.HostPaths = slices.Clone(s.Allow.HostPaths)
	out.Allow.Secrets = slices.Clone(s.Allow.Secrets)
	out.Allow.Configs = slices.Clone(s.Allow.Configs)
	out.Allow.Volumes = slices.Clone(s.Allow.Volumes)
	out.Allow.Networks = slices.Clone(s.Allow.Networks)

	if s.Source.Chart != nil {
		chart := *s.Source.Chart
		chart.Values = slices.Clone(s.Source.Chart.Values)
		chart.Repositories = slices.Clone(s.Source.Chart.Repositories)
		out.Source.Chart = &chart
	}
	return out
}

// cloneReleaseDrift is separate because it is the only pointee with references
// of its own; everything else clonePtr reaches is flat.
func cloneReleaseDrift(d *ReleaseDrift) *ReleaseDrift {
	if d == nil {
		return nil
	}
	out := *d
	out.Services = slices.Clone(d.Services)
	for i := range out.Services {
		out.Services[i].Fields = slices.Clone(d.Services[i].Fields)
	}
	out.Resources = slices.Clone(d.Resources)
	return &out
}

// clonePtr copies what a pointer points at, for a pointee whose own fields are
// all values. Nil stays nil, because nil is meaningful on every one of these:
// it is "not asked" rather than "asked and found nothing".
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	out := *p
	return &out
}

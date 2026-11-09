package server

import (
	"slices"
	"strconv"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-exporter/internal/history"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/libstats"
	"github.com/cplieger/plex-exporter/internal/metrics"
)

// itemTitle bounds an item title label: one line, no control or bidi runes,
// at most maxLabelLen bytes with the cut marked.
func itemTitle(s string) string {
	t, _ := textsafe.SanitizeSingleLineCapped(s, maxLabelLen, "...")
	return t
}

func yearLabel(y int) string {
	if y <= 0 {
		return ""
	}
	return strconv.Itoa(y)
}

func bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// collectCatalog emits the walk, history, activity, update and scan schedule
// families.
// Watch-age figures and last-played times publish only for a library walked
// while history was current at the generation still current now, and only
// while no history row is unmatched.
func collectCatalog(em *emitter, snap *snapshot) {
	srv, id := snap.Name, snap.ID
	walkable, skipped := walkableLibraries(snap.Libraries)
	hv := snap.history
	walked, stamped := emitLibraries(em, snap, walkable)
	allWalked := walked && snap.passDone && skipped == 0

	if snap.passDone {
		em.emit(metrics.DescWalkPassDuration, snap.passDuration.Seconds(), srv, id)
	}
	em.emit(metrics.DescWalkSkipped, float64(skipped), srv, id)
	em.emit(metrics.DescWalkComplete, bool01(allWalked), srv, id)
	if snap.historyEnabled {
		emitHistory(em, srv, id, hv)
		complete := hv.Current && hv.Unmatched == 0 && allWalked && stamped
		em.emit(metrics.DescWatchAgeComplete, bool01(complete), srv, id)
	}
	for _, sec := range snap.sections {
		if sec.scannedAt > 0 {
			em.emit(metrics.DescLastScan, float64(sec.scannedAt), srv, id, sec.typ, sec.id)
		}
	}
	for k, v := range snap.activities {
		em.emit(metrics.DescActivityProgress, v, srv, id, k.typ, k.libType, k.libID)
	}
	if u := snap.update; u.known {
		em.emit(metrics.DescUpdateAvailable, u.available, srv, id, u.state)
		em.emit(metrics.DescUpdateChecked, float64(u.checkedAt), srv, id)
	}
	if snap.scanInterval > 0 {
		em.emit(metrics.DescScanInterval, snap.scanInterval.Seconds(), srv, id)
	}
}

// emitLibraries emits each walkable library's figures and the server's
// newest items. walked reports every library has a walk whose last attempt
// succeeded; stamped that every one publishes watch figures.
func emitLibraries(em *emitter, snap *snapshot, walkable []library.Library) (walked, stamped bool) {
	hv := snap.history
	walked, stamped = true, true
	recent := make([][]libstats.Item, 0, len(walkable))
	for _, lib := range walkable {
		labels := []string{snap.Name, snap.ID, lib.Type, lib.ID}
		em.emit(metrics.DescWalkable, 1, labels...)
		w, ok := snap.walks[lib.ID]
		if !ok || !w.walked {
			walked, stamped = false, false
			continue
		}
		watch := snap.historyEnabled && hv.Current && hv.Unmatched == 0 && w.stamp != 0 && w.stamp == hv.Gen
		walked = walked && !w.failed
		stamped = stamped && watch
		emitLibraryWalk(em, labels, &w, watch)
		recent = append(recent, w.stats.Recent)
	}
	for _, it := range libstats.MergeRecent(recent...) {
		em.emit(metrics.DescRecentItemAdded, float64(it.AddedAt), snap.Name, snap.ID, it.LibraryType, it.LibraryID,
			strconv.FormatUint(it.RatingKey, 10), itemTitle(it.Title), yearLabel(it.Year), it.Episode)
	}
	return walked, stamped
}

func emitLibraryWalk(em *emitter, labels []string, w *libWalk, watch bool) {
	st := &w.stats
	em.emit(metrics.DescWalkLastSuccess, float64(w.lastSuccess.Unix()), labels...)
	em.emit(metrics.DescWalkDuration, w.duration.Seconds(), labels...)
	em.emit(metrics.DescUnsizedItems, float64(st.Unsized), labels...)
	em.emit(metrics.DescMultiVersionItems, float64(st.MultiVersionItems), labels...)
	em.emit(metrics.DescExtraVersionBytes, float64(st.ExtraVersionBytes), labels...)
	em.emit(metrics.DescAddedItems, float64(st.Added24h), with(labels, "24h")...)
	em.emit(metrics.DescAddedItems, float64(st.Added7d), with(labels, "7d")...)
	if st.NewestAdded > 0 {
		em.emit(metrics.DescNewestItemAdded, float64(st.NewestAdded), labels...)
	}
	for k, v := range st.ResolutionBytes {
		em.emit(metrics.DescResolutionBytes, float64(v), with(labels, k)...)
	}
	for k, v := range st.CodecBytes {
		em.emit(metrics.DescCodecBytes, float64(v), with(labels, k)...)
	}
	for _, it := range st.Largest {
		item := with(labels, strconv.FormatUint(it.RatingKey, 10), itemTitle(it.Title), yearLabel(it.Year))
		em.emit(metrics.DescTopItemBytes, float64(it.Bytes), item...)
		if watch {
			em.emit(metrics.DescTopItemLastPlayed, float64(it.LastPlayed), item...)
		}
	}
	if !watch {
		return
	}
	for _, b := range libstats.WatchBuckets {
		em.emit(metrics.DescWatchAgeBytes, float64(st.WatchAgeBytes[b]), with(labels, b)...)
		em.emit(metrics.DescWatchAgeItems, float64(st.WatchAgeItems[b]), with(labels, b)...)
	}
}

func emitHistory(em *emitter, srv, id string, hv history.View) {
	if hv.Status == "" {
		return
	}
	for _, st := range history.Statuses {
		em.emit(metrics.DescHistoryReadStatus, bool01(hv.Status == st), srv, id, string(st))
	}
	if !hv.LastSuccess.IsZero() {
		em.emit(metrics.DescHistoryLastSuccess, float64(hv.LastSuccess.Unix()), srv, id)
	}
	em.emit(metrics.DescHistoryUnmatched, float64(hv.Unmatched), srv, id)
	em.emit(metrics.DescHistoryDeleted, float64(hv.Deleted), srv, id)
}

// with returns labels followed by extra in a new slice.
func with(labels []string, extra ...string) []string {
	return slices.Concat(labels, extra)
}

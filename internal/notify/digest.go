package notify

import (
	"context"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// digestBuffer collects the events pending for one channel between flushes.
type digestBuffer struct {
	notification Notification
	items        []domain.NotificationEvent
}

// digestAdd buffers an event for each target channel and arms the flush timer
// on the first buffered item. Caller passes the already policy-checked
// targets (mute is applied before buffering).
func (d *Dispatcher) digestAdd(targets []Notification, ev domain.NotificationEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	empty := len(d.digestBuf) == 0
	for _, n := range targets {
		buf, ok := d.digestBuf[n.ID]
		if !ok {
			buf = &digestBuffer{notification: n}
			d.digestBuf[n.ID] = buf
		}
		buf.notification = n // refresh config/templates
		buf.items = append(buf.items, ev)
	}
	if empty && len(d.digestBuf) > 0 && d.digestTimer == nil {
		d.digestTimer = d.clock.AfterFunc(d.digest.Interval, func() {
			d.enqueue(func(ctx context.Context) { d.flushDigest(ctx, false) })
		})
	}
}

// flushDigest sends one summary message per buffered channel and clears the
// buffer. Quiet hours defer the flush: the timer is re-armed and items are
// kept until the window ends.
//
// final marks the shutdown flush, which never defers. Deferring there would
// re-arm a timer on a dispatcher that is about to stop and drop the batch with
// it — and losing a DOWN outright is worse than sending it a few hours inside
// the do-not-disturb window. The `!d.stopped` arm below already encoded that
// intent for the case where the flag happened to be set in time; final makes it
// deterministic rather than a race.
func (d *Dispatcher) flushDigest(ctx context.Context, final bool) {
	now := d.clock.Now()

	d.mu.Lock()
	d.digestTimer = nil
	if !final && d.quiet != nil && d.quiet.Contains(now) && len(d.digestBuf) > 0 && !d.stopped {
		// Hold the batch until quiet hours end.
		d.digestTimer = d.clock.AfterFunc(d.digest.Interval, func() {
			d.enqueue(func(ctx context.Context) { d.flushDigest(ctx, false) })
		})
		d.mu.Unlock()
		d.log.Info("digest deferred: quiet hours")
		return
	}
	buffers := d.digestBuf
	d.digestBuf = make(map[int64]*digestBuffer)
	d.mu.Unlock()

	for _, buf := range buffers {
		if len(buf.items) == 0 {
			continue
		}
		ev := domain.NotificationEvent{
			Kind:  domain.EventKindDigest,
			At:    now,
			Items: buf.items,
		}
		// Digest is a single fan-out target; reuse the isolated sender.
		if err := d.sendOne(ctx, buf.notification, ev); err != nil {
			d.log.Warn("digest delivery failed", "notification_id", buf.notification.ID, "error", err)
			// The batch is gone from the buffer and was never delivered. Say so
			// per monitor, because the alternative is an outage that produced no
			// message and no trace of having failed to.
			for _, item := range buf.items {
				if item.Kind == domain.EventKindDown {
					d.log.Error("notify: a DOWN alert was lost with a failed digest",
						"notification_id", buf.notification.ID, "monitor_id", item.MonitorID)
				}
			}
			continue
		}
		// Delivered — only NOW are these channels notified about these outages.
		//
		// Buffering used to count as reaching the channel, so a digest that
		// failed (or a process that died before the flush) left the DOWN
		// unsent while the channel was marked as having heard it, and the
		// eventual recovery went out to someone who was never told there was a
		// problem. markDigestDelivered moves that moment to where the message
		// actually left.
		d.markDigestDelivered(buf.notification.ID, buf.items)
	}
}

// markDigestDelivered records, after a successful flush, that this channel has
// now been told about each outage the batch carried.
//
// It writes a per-monitor audit row alongside the digest's own. That is not
// duplication: the digest row carries no monitor, so without these the delivery
// log cannot answer "was this channel told this monitor was down", which is the
// question Dispatcher.Restore asks after a restart. Without them a digest-mode
// deployment would lose every recovery across a restart.
func (d *Dispatcher) markDigestDelivered(notificationID int64, items []domain.NotificationEvent) {
	// One row per monitor, carrying its LAST word in this batch.
	//
	// Recording only the DOWN items left a digest-delivered recovery invisible
	// to the audit log, so the channel's last recorded word stayed "down" — and
	// a restart during the NEXT outage would then treat it as already told and
	// resolve to it, which is the defect ChannelsAwaitingRecovery exists to
	// prevent, reintroduced through the digest path.
	//
	// Last-word rather than one row per item, because a batch can carry a
	// down and an up for the same monitor and both would be stamped with the
	// same instant: "most recent delivery" would then be decided by whichever
	// row the database happened to return first.
	last := make(map[int64]domain.NotificationEventKind, len(items))
	order := make([]int64, 0, len(items))
	for _, item := range items {
		if item.MonitorID == 0 {
			continue
		}
		if item.Kind != domain.EventKindDown && item.Kind != domain.EventKindUp {
			continue
		}
		if _, ok := last[item.MonitorID]; !ok {
			order = append(order, item.MonitorID)
		}
		last[item.MonitorID] = item.Kind
	}

	for _, monitorID := range order {
		kind := last[monitorID]
		if kind == domain.EventKindDown {
			d.mu.Lock()
			if st := d.down[monitorID]; st != nil {
				st.notified[notificationID] = true
			}
			d.mu.Unlock()
		}

		ctx, cancel := context.WithTimeout(context.WithoutCancel(d.baseCtx), recordTimeout)
		err := d.store.RecordDelivery(ctx, Delivery{
			NotificationID: notificationID,
			MonitorID:      monitorID,
			ProviderType:   "digest",
			EventKind:      kind,
			Status:         DeliverySuccess,
			Attempts:       1,
			StartedAt:      d.clock.Now(),
			FinishedAt:     d.clock.Now(),
		})
		cancel()
		if err != nil {
			d.log.Error("notify: record digest delivery", "monitor_id", monitorID, "error", err)
		}
	}
}

// pendingDigestItems reports the number of buffered items for a channel
// (test helper; also handy for metrics later).
func (d *Dispatcher) pendingDigestItems(notificationID int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if buf, ok := d.digestBuf[notificationID]; ok {
		return len(buf.items)
	}
	return 0
}

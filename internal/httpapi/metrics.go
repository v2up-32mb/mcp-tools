package httpapi

import "sync/atomic"

type serverMetrics struct {
	asyncStreamAttempts          atomic.Int64
	asyncStreamPublished         atomic.Int64
	asyncStreamFallbacks         atomic.Int64
	resourceNotificationAttempts atomic.Int64
	resourceNotificationSent     atomic.Int64
	resourceNotificationDropped  atomic.Int64
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{}
}

func (m *serverMetrics) snapshot() map[string]int64 {
	return map[string]int64{
		"async_stream_attempts":             m.asyncStreamAttempts.Load(),
		"async_stream_published":            m.asyncStreamPublished.Load(),
		"async_stream_fallback_single_shot": m.asyncStreamFallbacks.Load(),
		"resource_notification_attempts":    m.resourceNotificationAttempts.Load(),
		"resource_notification_sent":        m.resourceNotificationSent.Load(),
		"resource_notification_dropped":     m.resourceNotificationDropped.Load(),
	}
}

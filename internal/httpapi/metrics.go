package httpapi

import (
	"sync"
	"sync/atomic"
)

type serverMetrics struct {
	asyncStreamAttempts          atomic.Int64
	asyncStreamPublished         atomic.Int64
	asyncStreamFallbacks         atomic.Int64
	resourceNotificationAttempts atomic.Int64
	resourceNotificationSent     atomic.Int64
	resourceNotificationDropped  atomic.Int64
	templateMu                   sync.Mutex
	templateAttempts             int64
	templateSuccess              int64
	templateFailure              int64
	templateCalls                map[string]int64
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{templateCalls: make(map[string]int64)}
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

func (m *serverMetrics) recordTemplateCall(name string, success bool) {
	m.templateMu.Lock()
	defer m.templateMu.Unlock()
	m.templateAttempts++
	m.templateCalls[name]++
	if success {
		m.templateSuccess++
	} else {
		m.templateFailure++
	}
}

func (m *serverMetrics) templateSnapshot() map[string]any {
	m.templateMu.Lock()
	defer m.templateMu.Unlock()
	perTemplate := make(map[string]int64, len(m.templateCalls))
	for k, v := range m.templateCalls {
		perTemplate[k] = v
	}
	return map[string]any{
		"attempts":     m.templateAttempts,
		"success":      m.templateSuccess,
		"failure":      m.templateFailure,
		"per_template": perTemplate,
	}
}

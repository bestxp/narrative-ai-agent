package api

import "github.com/bestxp/narrative-ai-agent/internal/domain"

// InfoRepository owns the bot registry (info.yaml).
// The registry has exactly one record per bot instance,
// so methods take no slug — the record is global.
type InfoRepository interface {
	Load() (domain.Info, error)
	Save(info domain.Info) error
}

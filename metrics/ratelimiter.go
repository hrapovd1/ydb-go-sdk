package metrics

import (
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"
)

func ratelimiter(config Config) trace.Ratelimiter {
	return trace.Ratelimiter{}
}

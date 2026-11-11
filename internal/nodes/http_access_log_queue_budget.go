package nodes

import (
	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/delivery"
)

// Include the enclosing batch's repeated log entry. Normal producer logs need
// only lengths and header-value iteration; unusual Node/unknown data uses the
// schema traversal so admission and restored-wire budgets agree.
func accessLogElementCount(log *pb.HTTPAccessLog) (int, error) {
	if log.Node != nil || len(log.ProtoReflect().GetUnknown()) > 0 {
		return reflectedAccessLogElementCount(log)
	}
	count := 1
	for _, n := range [...]int{len(log.Header), len(log.SentHeader), len(log.Cookie), len(log.Attrs), len(log.Errors), len(log.FirewallActions), len(log.Tags)} {
		if n > delivery.MaxBatchElements-count {
			return 0, delivery.ErrBatchElements
		}
		count += n
	}
	for _, headers := range [...]map[string]*pb.Strings{log.Header, log.SentHeader} {
		for _, values := range headers {
			if values == nil {
				continue
			}
			if len(values.ProtoReflect().GetUnknown()) > 0 {
				return reflectedAccessLogElementCount(log)
			}
			if len(values.Values) > delivery.MaxBatchElements-count {
				return 0, delivery.ErrBatchElements
			}
			count += len(values.Values)
		}
	}
	return count, nil
}

func reflectedAccessLogElementCount(log *pb.HTTPAccessLog) (int, error) {
	count, err := delivery.MessageElements(log.ProtoReflect(), 2)
	if err != nil {
		return 0, err
	}
	if count >= delivery.MaxBatchElements {
		return 0, delivery.ErrBatchElements
	}
	return count + 1, nil
}

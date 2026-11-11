package checkpoints

import (
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
	"github.com/TeaOSLab/EdgeNode/internal/waf/utils"
	"github.com/iwind/TeaGo/maps"
)

// ResponseBodyCheckpoint ${responseBody}
type ResponseBodyCheckpoint struct {
	Checkpoint
}

func (this *ResponseBodyCheckpoint) IsRequest() bool {
	return false
}

func (this *ResponseBodyCheckpoint) RequestValue(req requests.Request, param string, options maps.Map, ruleId int64) (value any, hasRequestBody bool, sysErr error, userErr error) {
	value = ""
	return
}

func (this *ResponseBodyCheckpoint) ResponseValue(req requests.Request, resp *requests.Response, param string, options maps.Map, ruleId int64) (value any, hasRequestBody bool, sysErr error, userErr error) {
	value = ""
	if resp != nil && resp.Response != nil && resp.Body != nil {
		if len(resp.BodyData) > 0 {
			value = string(resp.BodyData)
			return
		}
		body, err := requests.InspectResponseBody(resp.Response)
		if err != nil {
			sysErr = err
			return
		}
		resp.BodyData = body
		value = body
	}
	return
}

func (this *ResponseBodyCheckpoint) CacheLife() utils.CacheLife {
	return utils.CacheMiddleLife
}

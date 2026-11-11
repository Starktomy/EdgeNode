package nodes

import (
	"io"
	"net/http"
)

type limitedUploadBody struct {
	io.ReadCloser
	limit int64
}

func (body *limitedUploadBody) BodyLimit() int64 { return body.limit }

// prepareRequestBodyLimit runs before authentication and WAF read the body.
// Even unknown-length bodies remain bounded while streaming to an origin.
func (this *HTTPRequest) prepareRequestBodyLimit() error {
	if this.RawReq == nil || this.web == nil || this.web.RequestLimit == nil || !this.web.RequestLimit.IsOn {
		return nil
	}
	var limit = this.web.RequestLimit.MaxBodyBytes()
	if limit <= 0 {
		return nil
	}
	if this.RawReq.ContentLength > limit {
		return &http.MaxBytesError{Limit: limit}
	}
	if this.RawReq.Body == nil || this.RawReq.Body == http.NoBody {
		return nil
	}
	if current, ok := this.RawReq.Body.(interface{ BodyLimit() int64 }); ok && current.BodyLimit() > 0 && current.BodyLimit() <= limit {
		return nil
	}
	this.RawReq.Body = &limitedUploadBody{
		ReadCloser: http.MaxBytesReader(this.RawWriter, this.RawReq.Body, limit),
		limit:      limit,
	}
	return nil
}

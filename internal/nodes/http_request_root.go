package nodes

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	rangeutils "github.com/TeaOSLab/EdgeNode/internal/utils/ranges"
	"github.com/TeaOSLab/EdgeNode/internal/utils/zero"
	"github.com/cespare/xxhash/v2"
	"github.com/iwind/TeaGo/Tea"
	"github.com/iwind/TeaGo/logs"
	"github.com/iwind/TeaGo/types"
)

// 文本mime-type列表
var textMimeMap = map[string]zero.Zero{
	"application/atom+xml":                {},
	"application/javascript":              {},
	"application/x-javascript":            {},
	"application/json":                    {},
	"application/rss+xml":                 {},
	"application/x-web-app-manifest+json": {},
	"application/xhtml+xml":               {},
	"application/xml":                     {},
	"image/svg+xml":                       {},
	"text/css":                            {},
	"text/plain":                          {},
	"text/javascript":                     {},
	"text/xml":                            {},
	"text/html":                           {},
	"text/xhtml":                          {},
	"text/sgml":                           {},
}

// 调用本地静态资源
// 如果返回true，则终止请求
func (this *HTTPRequest) doRoot() (isBreak bool) {
	if this.web.Root == nil || !this.web.Root.IsOn {
		return
	}

	if len(this.uri) == 0 {
		this.write404()
		return true
	}

	var rootDir = this.web.Root.Dir
	if this.web.Root.HasVariables() {
		rootDir = this.Format(rootDir)
	}
	if !filepath.IsAbs(rootDir) {
		rootDir = Tea.Root + Tea.DS + rootDir
	}

	var requestPath = this.uri

	var questionMarkIndex = strings.Index(this.uri, "?")
	if questionMarkIndex > -1 {
		requestPath = this.uri[:questionMarkIndex]
	}

	relative, err := normalizeRootPath(requestPath, this.web.Root.DecodePath, this.web.Root.StripPrefix, this.web.Root.ExceptHiddenFiles)
	normalizedURLPath, canonicalErr := normalizeRootPath(requestPath, this.web.Root.DecodePath, "", this.web.Root.ExceptHiddenFiles)
	if normalizedURLPath == "." {
		normalizedURLPath = ""
	}
	var canonicalURL = this.requestScheme() + "://" + this.ReqHost + "/" + normalizedURLPath
	if questionMarkIndex > -1 {
		canonicalURL += this.uri[questionMarkIndex:]
	}
	if err != nil || canonicalErr != nil || !this.web.Root.MatchURL(this.URL()) || !this.web.Root.MatchURL(canonicalURL) {
		this.write404()
		return true
	}

	fileReader, relative, err := openRootFile(rootDir, relative, this.web.Root.Indexes, this.web.Root.ExceptHiddenFiles)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !this.web.Root.IsBreak {
			return false
		}
		this.write404()
		return true
	}
	defer fileReader.Close()
	stat, err := fileReader.Stat()
	if err != nil {
		this.write50x(err, http.StatusInternalServerError, "Failed to stat the file", "查看文件统计信息失败", true)
		return true
	}
	var filename = relative
	var filePath = filepath.Join(rootDir, filepath.FromSlash(relative))
	this.filePath = filePath

	// 响应header
	var respHeader = this.writer.Header()

	// mime type
	var contentType = ""
	if this.web.ResponseHeaderPolicy == nil || !this.web.ResponseHeaderPolicy.IsOn || !this.web.ResponseHeaderPolicy.ContainsHeader("CONTENT-TYPE") {
		var ext = filepath.Ext(filePath)
		if len(ext) > 0 {
			mimeType := mime.TypeByExtension(ext)
			if len(mimeType) > 0 {
				var semicolonIndex = strings.Index(mimeType, ";")
				var mimeTypeKey = mimeType
				if semicolonIndex > 0 {
					mimeTypeKey = mimeType[:semicolonIndex]
				}

				if _, found := textMimeMap[mimeTypeKey]; found {
					if this.web.Charset != nil && this.web.Charset.IsOn && len(this.web.Charset.Charset) > 0 {
						var charset = this.web.Charset.Charset
						if this.web.Charset.IsUpper {
							charset = strings.ToUpper(charset)
						}
						contentType = mimeTypeKey + "; charset=" + charset
						respHeader.Set("Content-Type", mimeTypeKey+"; charset="+charset)
					} else {
						contentType = mimeType
						respHeader.Set("Content-Type", mimeType)
					}
				} else {
					contentType = mimeType
					respHeader.Set("Content-Type", mimeType)
				}
			}
		}
	}

	// length
	var fileSize = stat.Size()

	// 支持 Last-Modified
	modifiedTime := stat.ModTime().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	if len(respHeader.Get("Last-Modified")) == 0 {
		respHeader.Set("Last-Modified", modifiedTime)
	}

	// 支持 ETag
	var eTag = "\"e" + fmt.Sprintf("%0x", xxhash.Sum64String(filename+strconv.FormatInt(stat.ModTime().UnixNano(), 10)+strconv.FormatInt(stat.Size(), 10))) + "\""
	if len(respHeader.Get("ETag")) == 0 {
		respHeader.Set("ETag", eTag)
	}

	// 调用回调
	this.onRequest()
	if this.writer.isFinished {
		return
	}

	// 支持 If-None-Match
	if this.requestHeader("If-None-Match") == eTag {
		// 自定义Header
		this.ProcessResponseHeaders(this.writer.Header(), http.StatusNotModified)
		this.writer.WriteHeader(http.StatusNotModified)
		return true
	}

	// 支持 If-Modified-Since
	if this.requestHeader("If-Modified-Since") == modifiedTime {
		// 自定义Header
		this.ProcessResponseHeaders(this.writer.Header(), http.StatusNotModified)
		this.writer.WriteHeader(http.StatusNotModified)
		return true
	}

	// 支持Range
	respHeader.Set("Accept-Ranges", "bytes")
	ifRangeHeaders, ok := this.RawReq.Header["If-Range"]
	var supportRange = true
	if ok {
		supportRange = false
		for _, v := range ifRangeHeaders {
			if v == eTag || v == modifiedTime {
				supportRange = true
				break
			}
		}
		if !supportRange {
			respHeader.Del("Accept-Ranges")
		}
	}

	// 支持Range
	var ranges = []rangeutils.Range{}
	if supportRange {
		var contentRange = this.RawReq.Header.Get("Range")
		if len(contentRange) > 0 {
			if fileSize == 0 {
				this.ProcessResponseHeaders(this.writer.Header(), http.StatusRequestedRangeNotSatisfiable)
				this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}

			set, ok := httpRequestParseRangeHeader(contentRange)
			if !ok {
				this.ProcessResponseHeaders(this.writer.Header(), http.StatusRequestedRangeNotSatisfiable)
				this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			if len(set) > 0 {
				ranges = set
				for k, r := range ranges {
					r2, ok := r.Convert(fileSize)
					if !ok {
						this.ProcessResponseHeaders(this.writer.Header(), http.StatusRequestedRangeNotSatisfiable)
						this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
						return true
					}
					ranges[k] = r2
				}
			}
		} else {
			respHeader.Set("Content-Length", strconv.FormatInt(fileSize, 10))
		}
	} else {
		respHeader.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	}

	// 自定义Header
	this.ProcessResponseHeaders(this.writer.Header(), http.StatusOK)

	// 在Range请求中不能缓存
	if len(ranges) > 0 {
		this.cacheRef = nil // 不支持缓存
	}

	var resp = &http.Response{
		ContentLength: fileSize,
		Body:          fileReader,
		StatusCode:    http.StatusOK,
	}
	this.writer.Prepare(resp, fileSize, http.StatusOK, true)

	var pool = this.bytePool(fileSize)
	var buf = pool.Get()
	defer func() {
		pool.Put(buf)
	}()

	if len(ranges) == 1 {
		respHeader.Set("Content-Range", ranges[0].ComposeContentRangeHeader(types.String(fileSize)))
		this.writer.WriteHeader(http.StatusPartialContent)

		ok, err := httpRequestReadRange(resp.Body, buf.Bytes, ranges[0].Start(), ranges[0].End(), func(buf []byte, n int) error {
			_, err := this.writer.Write(buf[:n])
			return err
		})
		if err != nil {
			if !this.canIgnore(err) {
				logs.Error(err)
			}
			return true
		}
		if !ok {
			this.ProcessResponseHeaders(this.writer.Header(), http.StatusRequestedRangeNotSatisfiable)
			this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return true
		}
	} else if len(ranges) > 1 {
		var boundary = httpRequestGenBoundary()
		respHeader.Set("Content-Type", "multipart/byteranges; boundary="+boundary)

		this.writer.WriteHeader(http.StatusPartialContent)

		for index, r := range ranges {
			if index == 0 {
				_, err = this.writer.WriteString("--" + boundary + "\r\n")
			} else {
				_, err = this.writer.WriteString("\r\n--" + boundary + "\r\n")
			}
			if err != nil {
				if !this.canIgnore(err) {
					logs.Error(err)
				}
				return true
			}

			_, err = this.writer.WriteString("Content-Range: " + r.ComposeContentRangeHeader(types.String(fileSize)) + "\r\n")
			if err != nil {
				if !this.canIgnore(err) {
					logs.Error(err)
				}
				return true
			}

			if len(contentType) > 0 {
				_, err = this.writer.WriteString("Content-Type: " + contentType + "\r\n\r\n")
				if err != nil {
					if !this.canIgnore(err) {
						logs.Error(err)
					}
					return true
				}
			}

			ok, err := httpRequestReadRange(resp.Body, buf.Bytes, r.Start(), r.End(), func(buf []byte, n int) error {
				_, err := this.writer.Write(buf[:n])
				return err
			})
			if err != nil {
				if !this.canIgnore(err) {
					logs.Error(err)
				}
				return true
			}
			if !ok {
				this.ProcessResponseHeaders(this.writer.Header(), http.StatusRequestedRangeNotSatisfiable)
				this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
		}

		_, err = this.writer.WriteString("\r\n--" + boundary + "--\r\n")
		if err != nil {
			if !this.canIgnore(err) {
				logs.Error(err)
			}
			return true
		}
	} else {
		_, err = io.CopyBuffer(this.writer, resp.Body, buf.Bytes)
		if err != nil {
			if !this.canIgnore(err) {
				logs.Error(err)
			}
			return true
		}
	}

	// 设置成功
	this.writer.SetOk()

	return true
}

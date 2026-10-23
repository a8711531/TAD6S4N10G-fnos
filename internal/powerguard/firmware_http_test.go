package powerguard

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归（2026-10-23）：handleBIOSOnlineFlash 曾显式 r.ParseForm()，
// Go 的 ParseForm 不解析 multipart 请求体，还会把 r.Form 置成非 nil，
// 导致后续 FormValue 跳过惰性 ParseMultipartForm——前端 FormData 提交的
// confirm=FLASH 永远取不到，刷写被误拒「缺少刷写确认」。

func biosOnlineFlashRequest(t *testing.T, contentType string, body *bytes.Buffer) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/firmware/bios-online/flash", body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-Trim-Isadmin", "true")
	return request
}

func assertBIOSOnlineConfirmAccepted(t *testing.T, request *http.Request) {
	t.Helper()
	server := &Server{Manager: &Manager{}}
	recorder := httptest.NewRecorder()
	server.handleBIOSOnlineFlash(recorder, request)
	if strings.Contains(recorder.Body.String(), "缺少刷写确认") {
		t.Fatalf("confirm=FLASH 未被识别（multipart 解析回归）: %s", recorder.Body.String())
	}
	// 确认通过后才会走到候选检查：未执行「检查更新」时应报这个错
	if !strings.Contains(recorder.Body.String(), "检查更新") {
		t.Fatalf("确认通过后应提示先检查更新，实际 %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleBIOSOnlineFlashAcceptsMultipartConfirm(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("confirm", "FLASH"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertBIOSOnlineConfirmAccepted(t, biosOnlineFlashRequest(t, writer.FormDataContentType(), &body))
}

func TestHandleBIOSOnlineFlashAcceptsURLEncodedConfirm(t *testing.T) {
	body := bytes.NewBufferString("confirm=FLASH")
	assertBIOSOnlineConfirmAccepted(t, biosOnlineFlashRequest(t, "application/x-www-form-urlencoded", body))
}

func TestHandleBIOSOnlineFlashRejectsMissingConfirm(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: &Manager{}}
	recorder := httptest.NewRecorder()
	server.handleBIOSOnlineFlash(recorder, biosOnlineFlashRequest(t, writer.FormDataContentType(), &body))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "缺少刷写确认") {
		t.Fatalf("缺少 confirm 应拒绝，实际 %d %s", recorder.Code, recorder.Body.String())
	}
}

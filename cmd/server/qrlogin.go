package main

// 微信扫码登录（复刻 weread.qq.com 官方网页版登录协议，纯标准库）：
//
//	POST /api/cookie/qr           创建（或复用）扫码会话，返回二维码内容 qrUrl
//	GET  /api/cookie/qr/status    查询扫码状态（waiting / otp / success / expired）
//	POST /api/cookie/qr/otp       扫码后需要 4 位验证码时提交（{otp}）
//
// 协议流程（与官方登录页一致）：
//	1. GET  https://weread.qq.com/api/auth/getLoginUid            → {uid}
//	2. 二维码内容 = https://weread.qq.com/web/confirm?uid=<uid>（手机微信扫码打开确认页）
//	3. 轮询 GET https://weread.qq.com/api/auth/getLoginInfo?uid=<uid>[&otp=xxxx]
//	   确认后响应含 webLoginVid / accessToken，且经 Set-Cookie 下发正式会话（wr_vid / wr_skey）
//	4. 汇总全部会话 Cookie 写入 cookie.txt，与手动粘贴完全等效

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	qrUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	qrBaseURL   = "https://weread.qq.com"
	qrTTL       = 5 * time.Minute // 扫码会话有效期，超时视为过期
)

type qrSession struct {
	mu      sync.Mutex
	uid     string
	qrURL   string
	otp     string
	state    string // waiting / otp / success / expired
	jar     *cookiejar.Jar
	created time.Time
}

var (
	qrMu  sync.Mutex
	qrCur *qrSession
)

func qrHTTPClient(jar *cookiejar.Jar) *http.Client {
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

func qrDo(c *http.Client, rawURL string) (string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", qrUserAgent)
	req.Header.Set("Referer", qrBaseURL+"/")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(body), err
}

// qrBuildCookie 汇总会话 Cookie：优先取 Set-Cookie 落进 jar 的正式键，
// 缺失时用响应体的 webLoginVid / accessToken 兜底补齐 wr_vid / wr_skey。
func qrBuildCookie(jar *cookiejar.Jar, vid, token string) string {
	u, _ := url.Parse(qrBaseURL)
	m := map[string]string{}
	for _, c := range jar.Cookies(u) {
		m[c.Name] = c.Value
	}
	if vid != "" {
		if _, ok := m["wr_vid"]; !ok {
			m["wr_vid"] = vid
		}
	}
	if token != "" {
		if _, ok := m["wr_skey"]; !ok {
			m["wr_skey"] = token
		}
	}
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+m[k])
	}
	return "Cookie: " + strings.Join(parts, "; ")
}

// handleQRStart 创建（或复用未过期的）扫码会话
func handleQRStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	qrMu.Lock()
	defer qrMu.Unlock()
	// 已有待扫码会话且未过期：直接复用（避免重复取 uid）
	if qrCur != nil {
		qrCur.mu.Lock()
		reusable := (qrCur.state == "waiting" || qrCur.state == "otp") && time.Since(qrCur.created) < qrTTL
		st, qURL := qrCur.state, qrCur.qrURL
		qrCur.mu.Unlock()
		if reusable {
			writeJSON(w, map[string]string{"qrUrl": qURL, "state": st})
			return
		}
	}
	jar, _ := cookiejar.New(nil)
	client := qrHTTPClient(jar)
	body, err := qrDo(client, qrBaseURL+"/api/auth/getLoginUid")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "无法连接微信读书: "+err.Error())
		return
	}
	var u struct {
		UID string `json:"uid"`
	}
	if json.Unmarshal([]byte(body), &u) != nil || u.UID == "" {
		writeErr(w, http.StatusBadGateway, "获取登录 uid 失败: "+body)
		return
	}
	s := &qrSession{
		uid:     u.UID,
		qrURL:   qrBaseURL + "/web/confirm?uid=" + url.QueryEscape(u.UID),
		state:   "waiting",
		jar:     jar,
		created: time.Now(),
	}
	qrCur = s
	go qrPollLoop(s)
	log.Println("扫码登录会话已创建，二维码内容:", s.qrURL)
	writeJSON(w, map[string]string{"qrUrl": s.qrURL, "state": s.state})
}

// qrPollLoop 后台轮询扫码状态，直到成功或会话过期
func qrPollLoop(s *qrSession) {
	client := qrHTTPClient(s.jar)
	for time.Since(s.created) < qrTTL {
		s.mu.Lock()
		otp, st := s.otp, s.state
		s.mu.Unlock()
		if st == "success" {
			return
		}
		u := qrBaseURL + "/api/auth/getLoginInfo?uid=" + url.QueryEscape(s.uid)
		if otp != "" {
			u += "&otp=" + url.QueryEscape(otp)
		}
		body, err := qrDo(client, u)
		if err == nil {
			var m map[string]interface{}
			json.Unmarshal([]byte(body), &m)
			vid, hasVid := m["webLoginVid"].(string)
			token, hasTok := m["accessToken"].(string)
			if hasVid || hasTok || strings.Contains(body, "webLoginVid") {
				cookieStr := qrBuildCookie(s.jar, vid, token)
				if cookieStr != "" {
					if err := os.WriteFile("cookie.txt", []byte(cookieStr), 0600); err != nil {
						s.mu.Lock()
						s.state = "expired"
						s.mu.Unlock()
						log.Println("扫码登录成功但写入 cookie.txt 失败:", err)
						return
					}
					s.mu.Lock()
					s.state = "success"
					s.mu.Unlock()
					log.Println("扫码登录成功，凭证已写入 cookie.txt")
					return
				}
			}
			// 服务端要求 4 位验证码（手机上显示）
			if strings.Contains(strings.ToUpper(body), "OTP") {
				s.mu.Lock()
				if s.state != "success" {
					s.state = "otp"
				}
				s.mu.Unlock()
			}
		}
		time.Sleep(1 * time.Second)
	}
	s.mu.Lock()
	if s.state != "success" {
		s.state = "expired"
	}
	s.mu.Unlock()
}

// handleQRStatus 查询扫码会话状态
func handleQRStatus(w http.ResponseWriter, r *http.Request) {
	qrMu.Lock()
	s := qrCur
	qrMu.Unlock()
	if s == nil {
		writeJSON(w, map[string]string{"state": "none"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]string{"state": s.state, "qrUrl": s.qrURL})
}

// handleQROtp 提交扫码后所需的 4 位验证码
func handleQROtp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		OTP string `json:"otp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	qrMu.Lock()
	s := qrCur
	qrMu.Unlock()
	if s == nil {
		writeErr(w, http.StatusNotFound, "没有进行中的扫码会话")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != "otp" && s.state != "waiting" {
		writeErr(w, http.StatusConflict, "当前会话不在等待验证码状态")
		return
	}
	s.otp = strings.TrimSpace(req.OTP)
	s.state = "waiting"
	writeJSON(w, map[string]bool{"ok": true})
}

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gridea-pro/backend/internal/domain"
)

// fakeSettingRepoForSHA 让 httpClient() 进入 proxied client 分支：
// 生产代码里 proxyURL=="" 时会返回 http.DefaultClient（忽略 cachedClient），
// 只有 ProxyEnabled + ProxyURL 非空才会走"cachedProxyURL 匹配 → 复用 cachedClient"路径。
// 给个假的 ProxyURL="test" + 预置 cachedProxyURL="test" 把 httptest 劫持钩进去。
type fakeSettingRepoForSHA struct{}

func (fakeSettingRepoForSHA) GetSetting(ctx context.Context) (domain.Setting, error) {
	return domain.Setting{ProxyEnabled: true, ProxyURL: "test"}, nil
}

func (fakeSettingRepoForSHA) SaveSetting(ctx context.Context, _ domain.Setting) error {
	return nil
}

// newSHATestService 返回一个最小可用的 CdnUploadService，其 http client 经过
// rewriteTransport 劫持 —— 任何目标 URL 都会被打到 httptest 服务器上。
// 这样 getGithubFileSHA 内部硬编码的 api.github.com 可以在测试中被替换。
func newSHATestService(target string) *CdnUploadService {
	svc := &CdnUploadService{
		settingRepo: fakeSettingRepoForSHA{},
	}
	svc.cachedClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteTransport{to: target},
	}
	svc.cachedProxyURL = "test"
	return svc
}

// 200 + 有 sha：返回 (sha, nil) 供 uploadToGitHub 进入"对比跳过"路径
func TestGetGithubFileSHA_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
	}))
	defer srv.Close()

	svc := newSHATestService(srv.URL)
	setting := domain.CdnSetting{GithubUser: "u", GithubRepo: "r", GithubToken: "t"}

	sha, err := svc.getGithubFileSHA(context.Background(), setting, "x.png", "main")
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if sha != "deadbeef" {
		t.Errorf("want sha=deadbeef, got %q", sha)
	}
}

// 404：明确的"文件不存在"信号 → 调用方可以安全走 create 路径
func TestGetGithubFileSHA_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	svc := newSHATestService(srv.URL)
	setting := domain.CdnSetting{GithubUser: "u", GithubRepo: "r", GithubToken: "t"}

	_, err := svc.getGithubFileSHA(context.Background(), setting, "x.png", "main")
	if !errors.Is(err, errRemoteFileNotFound) {
		t.Errorf("404 should yield errRemoteFileNotFound, got %v", err)
	}
}

// 核心回归：撞二级限流（403）/ 429 / 5xx 时，必须返回非 nil 且非 errRemoteFileNotFound
// 的错误。否则 uploadToGitHub 会把它当成"文件不存在"，走 create 路径对已有文件
// 发无 sha PUT —— 触发 GitHub 422（正是 issue 报告的 22% 失败率的根因）。
func TestGetGithubFileSHA_RateLimited_DoesNotMasqueradeAsMissing(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"secondary_rate_limit_403", http.StatusForbidden},
		{"rate_limit_429", http.StatusTooManyRequests},
		{"server_error_502", http.StatusBadGateway},
		{"server_error_503", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "denied", tc.status)
			}))
			defer srv.Close()

			svc := newSHATestService(srv.URL)
			setting := domain.CdnSetting{GithubUser: "u", GithubRepo: "r", GithubToken: "t"}

			_, err := svc.getGithubFileSHA(context.Background(), setting, "x.png", "main")
			if err == nil {
				t.Fatal("expected error on non-200 non-404 response")
			}
			if errors.Is(err, errRemoteFileNotFound) {
				t.Errorf("%d must NOT masquerade as errRemoteFileNotFound", tc.status)
			}
			if !strings.Contains(err.Error(), "SHA") && !strings.Contains(err.Error(), "查询") {
				t.Errorf("error message should reference SHA/查询 context, got %q", err.Error())
			}
		})
	}
}

// 核心回归 #2：stream cancel / 5xx / 403 等瞬时错误应该在 getGithubFileSHA 内部
// 透明重试。前两次 503、第三次 200 模拟 GitHub secondary rate limit 恢复过程。
func TestGetGithubFileSHA_RetriesThroughTransientFailures(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			http.Error(w, "rate limited", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"cafe"}`))
	}))
	defer srv.Close()

	svc := newSHATestService(srv.URL)
	setting := domain.CdnSetting{GithubUser: "u", GithubRepo: "r", GithubToken: "t"}

	sha, err := svc.getGithubFileSHA(context.Background(), setting, "x.png", "main")
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if sha != "cafe" {
		t.Errorf("got sha %q, want cafe", sha)
	}
	if hits != 3 {
		t.Errorf("expected 3 attempts, got %d", hits)
	}
}

// rewriteTransport 把所有请求的 scheme+host 替换成 to，保留 path + query。
// httptest 给的是随机端口 http://127.0.0.1:xxxx —— 不 patch 代码就只能靠 transport
// 改写目的地，这样就不用把 api.github.com 的基址提出来做配置。
type rewriteTransport struct {
	to string
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	if i := strings.Index(r.to, "://"); i >= 0 {
		u.Scheme = r.to[:i]
		u.Host = r.to[i+3:]
	}
	req2 := req.Clone(req.Context())
	req2.URL = &u
	req2.Host = u.Host
	return http.DefaultTransport.RoundTrip(req2)
}

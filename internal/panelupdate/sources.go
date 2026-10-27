// 面板在线更新:只从本仓库(ttbb1211/kunpanel)的 GitHub Releases 检查与下载。
//
// 本仓库是定制 fork:一键更新绝不能取上游包,否则会用官方原版覆盖 README 顶部
// 列出的 4 处定制(2026-10-27 定)。上游三源(CNB/GitCode/Gitee)的实现保留在
// upstreamSources 里,仅供人工对照上游版本,不参与检查与安装。
package panelupdate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunpanel/internal/notifyx"
)

// 本仓库(唯一安装源)与上游三个发布平台的匿名直链
const (
	// githubRepo 本仓库:面板的检查与一键更新只认这里
	githubRepo = "ttbb1211/kunpanel"

	// 以下三项仅用于人工对照上游版本(见 upstreamSources),不参与安装
	cnbRepo        = "cnb.cool/kunge-nexa/kunpanel"
	gitcodeAPI     = "https://gitcode.com/api/v5/repos/NEXAB/kunpanel"
	gitcodeDistRef = "dist" // GitCode 附件走 dist 分支的仓库内文件(整文件或 .partNNN 分块)
	giteeRepo      = "NEXAB/kunpanel"
)

const uaHeader = "kunpanel-updater"

var (
	mu        sync.Mutex
	lastInfo  *Info
	current   string
	client    = &http.Client{Timeout: 30 * time.Second}
	bigClient = &http.Client{Timeout: 20 * time.Minute}
)

// Info 更新检查结果
type Info struct {
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	HasUpdate   bool   `json:"has_update"`
	Name        string `json:"name"`
	Notes       string `json:"notes"`
	PublishedAt string `json:"published_at"`
	CheckedAt   string `json:"checked_at"`
	Source      string `json:"source"` // 本次检查命中的更新源
}

func SetCurrent(v string) { current = v }

// httpGet 带 UA 的 GET;statusCode 非 200 返回错误
func httpGet(url string, timeout time.Duration) (*http.Response, error) {
	c := client
	if timeout > time.Minute {
		c = bigClient
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", uaHeader)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func httpGetString(url string, limit int64) (string, error) {
	resp, err := httpGet(url, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if limit <= 0 {
		limit = 1 << 20
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type release struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
}

// ---------- 本仓库(GitHub):唯一的检查与安装源 ----------

// githubLatest 本仓库最新 Release。匿名调用 api.github.com 限 60 次/小时,
// 面板每 5 分钟 + 每 6 小时各查一次(约 13 次/小时),留有余量。
func githubLatest() (*release, error) {
	s, err := httpGetString("https://api.github.com/repos/"+githubRepo+"/releases/latest", 1<<20)
	if err != nil {
		return nil, err
	}
	var r release
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("解析 Release 失败: %v", err)
	}
	if r.TagName == "" {
		return nil, fmt.Errorf("无 tag_name")
	}
	return &r, nil
}

// githubAssetURL Release 资产直链(会 302 到 objects.githubusercontent.com)
func githubAssetURL(tag, asset string) string {
	return "https://github.com/" + githubRepo + "/releases/download/" + tag + "/" + asset
}

// ---------- 上游三源(仅人工对照版本用,不参与检查与安装) ----------

// cnbLatest CNB 最新版本(匿名,Accept 需带 vnd.cnb.api+json)
func cnbLatest() (*release, error) {
	req, _ := http.NewRequest("GET", "https://"+cnbRepo+"/-/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.cnb.api+json")
	req.Header.Set("User-Agent", uaHeader)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var r release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, err
	}
	if r.TagName == "" {
		return nil, fmt.Errorf("无 tag_name")
	}
	return &r, nil
}

func cnbAssetURL(tag, asset string) string {
	return "https://" + cnbRepo + "/-/releases/download/" + tag + "/" + asset
}

// gitcodeLatest GitCode 最新版本:优先 releases 列表(新建仓库可能被内容审核
// 拦截而建不出 release),失败回退 tags 列表按语义化版本取最大
func gitcodeLatest() (*release, error) {
	s, err := httpGetString(gitcodeAPI+"/releases", 2<<20)
	if err == nil {
		var rs []release
		if json.Unmarshal([]byte(s), &rs) == nil && len(rs) > 0 && rs[0].TagName != "" {
			return &rs[0], nil
		}
	}
	s, err = httpGetString(gitcodeAPI+"/tags", 1<<20)
	if err != nil {
		return nil, err
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(s), &tags) != nil || len(tags) == 0 {
		return nil, fmt.Errorf("无 release 且无 tag")
	}
	best := ""
	for _, t := range tags {
		if _, ok := Semver(t.Name); ok && newer(t.Name, best) {
			best = t.Name
		}
	}
	if best == "" {
		return nil, fmt.Errorf("tag 均非语义化版本")
	}
	return &release{TagName: best, Name: best}, nil
}

// gitcodeFileBytes 经 contents API 读 dist 分支仓库内文件(匿名可读,base64)
func gitcodeFileBytes(path string) ([]byte, error) {
	s, err := httpGetString(gitcodeAPI+"/contents/"+path+"?ref="+gitcodeDistRef, 8<<20)
	if err != nil {
		return nil, err
	}
	var obj struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, err
	}
	if obj.Content == "" {
		return nil, fmt.Errorf("空内容")
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(obj.Content, "\n", ""))
}

// gitcodeDownload 下载 dist 分支文件:>1MB 的安装包在仓库内以 .partNNN 分块存放,
// 此处逐块拼接还原;≤1MB(如 SHA256SUMS)整文件直取。
func gitcodeDownload(path string, dst string, w io.Writer) error {
	if b, err := gitcodeFileBytes(path); err == nil {
		return os.WriteFile(dst, b, 0o755)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	total := 0
	for i := 0; i < 200; i++ {
		b, err := gitcodeFileBytes(fmt.Sprintf("%s.part%03d", path, i))
		if err != nil {
			if i == 0 {
				return fmt.Errorf("GitCode 上不存在 %s(整文件与分块均未找到)", path)
			}
			break
		}
		if _, werr := f.Write(b); werr != nil {
			return werr
		}
		total += len(b)
		if total > 512<<20 {
			return fmt.Errorf("分块超过 512MB,异常中止")
		}
	}
	fmt.Fprintf(w, "GitCode 分块下载完成(%d 字节)\n", total)
	return nil
}

func giteeLatest() (*release, error) {
	s, err := httpGetString("https://gitee.com/api/v5/repos/"+giteeRepo+"/releases/latest", 1<<20)
	if err != nil {
		return nil, err
	}
	var r release
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, err
	}
	if r.TagName == "" {
		return nil, fmt.Errorf("无 tag_name")
	}
	return &r, nil
}

func giteeAssetURL(tag, asset string) string {
	return "https://gitee.com/" + giteeRepo + "/releases/download/" + tag + "/" + asset
}

// updateSource 更新源描述
type updateSource struct {
	name     string
	latest   func() (*release, error)
	sums     func(tag string) (string, error)                // SHA256SUMS 文本
	download func(tag, asset, dst string, w io.Writer) error // 安装包下载
}

// sources 一键更新实际使用的源:只有本仓库。
// ⚠️ 别把 upstreamSources 里的项加进来 —— 那会让一键更新装上官方原版,
// 覆盖 README 顶部的 4 处定制。要对照上游版本请用 upstreamSources 单独查。
var sources = []updateSource{
	{
		name:   "GitHub",
		latest: githubLatest,
		sums: func(tag string) (string, error) {
			return httpGetString(githubAssetURL(tag, "SHA256SUMS"), 1<<20)
		},
		download: func(tag, asset, dst string, w io.Writer) error {
			return downloadFile(githubAssetURL(tag, asset), dst, w)
		},
	},
}

// upstreamSources 上游三源(CNB → GitCode → Gitee)。默认不参与任何流程,
// 仅供排障时人工对照上游是否已出新版。加进 sources 之前先想清楚后果。
var upstreamSources = []updateSource{
	{
		name:   "CNB",
		latest: cnbLatest,
		sums: func(tag string) (string, error) {
			return httpGetString(cnbAssetURL(tag, "SHA256SUMS"), 1<<20)
		},
		download: func(tag, asset, dst string, w io.Writer) error {
			return downloadFile(cnbAssetURL(tag, asset), dst, w)
		},
	},
	{
		name:   "GitCode",
		latest: gitcodeLatest,
		sums: func(tag string) (string, error) {
			b, err := gitcodeFileBytes("dist/assets/" + tag + "/SHA256SUMS")
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
		download: func(tag, asset, dst string, w io.Writer) error {
			return gitcodeDownload("dist/assets/"+tag+"/"+asset, dst, w)
		},
	},
	{
		name:   "Gitee",
		latest: giteeLatest,
		sums: func(tag string) (string, error) {
			return httpGetString(giteeAssetURL(tag, "SHA256SUMS"), 1<<20)
		},
		download: func(tag, asset, dst string, w io.Writer) error {
			return downloadFile(giteeAssetURL(tag, asset), dst, w)
		},
	},
}

// ---------- 检查 ----------

// Check 依次尝试各更新源(当前只有本仓库 GitHub),第一个成功者生效
func Check() (*Info, error) {
	var lastErr error
	for _, src := range sources {
		r, err := src.latest()
		if err != nil {
			lastErr = err
			continue
		}
		mu.Lock()
		lastInfo = &Info{
			Current:     current,
			Latest:      r.TagName,
			HasUpdate:   newer(r.TagName, current),
			Name:        r.Name,
			Notes:       r.Body,
			PublishedAt: r.PublishedAt,
			CheckedAt:   time.Now().Format("2006-01-02 15:04:05"),
			Source:      src.name,
		}
		info := lastInfo
		mu.Unlock()
		return info, nil
	}
	return nil, fmt.Errorf("更新源(%s)不可达: %v", githubRepo, lastErr)
}

// Cached 上次检查结果(可能为 nil)
func Cached() *Info {
	mu.Lock()
	defer mu.Unlock()
	return lastInfo
}

// Start 定时检查:启动 5 分钟后一次,之后每 6 小时一次;发现新版本写消息中心
func Start(stop <-chan struct{}) {
	go func() {
		time.Sleep(5 * time.Minute)
		if info, err := Check(); err == nil && info != nil && info.HasUpdate {
			notifyx.Add("update", "发现面板新版本 "+info.Latest,
				"当前 "+info.Current+",可前往 面板设置 → 关于 → 面板更新 一键在线更新")
		}
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if info, err := Check(); err == nil && info != nil && info.HasUpdate {
					notifyx.Add("update", "发现面板新版本 "+info.Latest,
						"当前 "+info.Current+",可前往 面板设置 → 关于 → 面板更新 一键在线更新")
				}
			}
		}
	}()
}

// ---------- 版本比较 ----------

// newer 判断 latest 是否严格大于 current(v 前缀语义化版本;解析失败退化为不等即新)
func newer(latest, current string) bool {
	l, ok1 := Semver(latest)
	c, ok2 := Semver(current)
	if !ok1 || !ok2 {
		return latest != "" && latest != current
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// Semver 解析 v 前缀语义化版本,非法返回 false
func Semver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || parts[0] == "" {
		return out, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ---------- 下载与校验 ----------

// expectedHash 从 SHA256SUMS 里取目标资产的哈希(行格式:<hash>  ./<name> 或 <hash>  <name>)
func expectedHash(sums, asset string) string {
	for _, ln := range strings.Split(sums, "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && (f[1] == "./"+asset || f[1] == asset || strings.HasSuffix(f[1], "/"+asset)) {
			if len(f[0]) == 64 {
				return strings.ToLower(f[0])
			}
		}
	}
	return ""
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func assetName(tag string) string {
	name := fmt.Sprintf("kunpanel_%s_%s_%s", tag, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".zip"
	} else {
		name += ".tar.gz"
	}
	return name
}

func downloadFile(url, dst string, w io.Writer) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", uaHeader)
	resp, err := bigClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return fmt.Errorf("下载中断: %v", err)
	}
	fmt.Fprintf(w, "已下载 %.1f MB\n", float64(n)/1024/1024)
	if n < 1<<20 {
		return fmt.Errorf("下载内容异常(小于 1MB)")
	}
	return nil
}

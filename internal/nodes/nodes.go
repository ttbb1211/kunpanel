// nodes —— 多节点管理(对标 1Panel 专业版「节点管理」,内置免费)。
//
// 与 1Panel 的差异:不部署常驻 Agent,直接 SSH 到对端执行只读采集命令
// (复用 1Panel/宝塔远程迁移同款通道)。节点通常就是另一台装了坤面板的
// 服务器:检测到面板时提供打开面板/重启面板/一键安装入口。
//
// 安全基线:
//   - 主机指纹 TOFU:首次添加回显,入库后强校验,漂移即拒连(防劫持)
//   - 凭据仅存本地 data/nodes.json(0600,与其他远端凭据同策略)
//   - 采集脚本为固定常量(base64 下发),用户输入不进入远端 shell
package nodes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"kunpanel/internal/config"
	"kunpanel/internal/util"
)

// Node 一台受管服务器
type Node struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	Host        string `json:"host"`
	Port        int    `json:"port"` // SSH 端口
	User        string `json:"user"`
	Password    string `json:"password,omitempty"` // Get/ 列表脱敏为空
	Fingerprint string `json:"fingerprint"`        // SSH 主机指纹(SHA256,TOFU 后强校验)
	Desc        string `json:"desc,omitempty"`
	CreatedAt   string `json:"created_at"`
	PasswordSet bool   `json:"password_set"` // Get 返回:密码是否已配置
}

// Snapshot 一次采集得到的节点指标
type Snapshot struct {
	Online     bool    `json:"online"`
	Error      string  `json:"error,omitempty"`
	Hostname   string  `json:"hostname,omitempty"`
	OS         string  `json:"os,omitempty"`
	Kernel     string  `json:"kernel,omitempty"`
	Cores      int     `json:"cores,omitempty"`
	CPUPercent float64 `json:"cpu_percent"`         // 双采样差分
	MemTotal   uint64  `json:"mem_total,omitempty"` // 字节
	MemUsed    uint64  `json:"mem_used,omitempty"`
	SwapTotal  uint64  `json:"swap_total,omitempty"`
	SwapUsed   uint64  `json:"swap_used,omitempty"`
	DiskTotal  uint64  `json:"disk_total,omitempty"` // 真实块设备按挂载点求和
	DiskUsed   uint64  `json:"disk_used,omitempty"`
	Load1      float64 `json:"load1,omitempty"`
	UptimeSec  uint64  `json:"uptime_sec,omitempty"`
	NetRx      float64 `json:"net_rx"` // B/s 双采样差分
	NetTx      float64 `json:"net_tx"`
	// 对端面板探测结果
	PanelInstalled bool   `json:"panel_installed"`
	PanelPort      int    `json:"panel_port,omitempty"`
	PanelEntrance  string `json:"panel_entrance,omitempty"`
	PanelSSL       bool   `json:"panel_ssl,omitempty"`
	CheckedAt      string `json:"checked_at"`
}

type store struct {
	Nodes []Node `json:"nodes"`
}

var lock sync.Mutex

func cfgPath() string { return filepath.Join(config.DataDir(), "nodes.json") }

func loadLocked() store {
	b, err := os.ReadFile(cfgPath())
	if err != nil {
		return store{}
	}
	var st store
	if json.Unmarshal(b, &st) != nil {
		return store{}
	}
	return st
}

func saveLocked(st store) error {
	for i := range st.Nodes {
		st.Nodes[i].PasswordSet = st.Nodes[i].Password != ""
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return util.AtomicWrite(cfgPath(), b, 0o600)
}

// List 节点列表(密码脱敏)
func List() []Node {
	nodes := listPlain()
	for i := range nodes {
		nodes[i].Password = ""
	}
	return nodes
}

// listPlain 内部用:含密码的完整列表
func listPlain() []Node {
	lock.Lock()
	defer lock.Unlock()
	st := loadLocked()
	out := make([]Node, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// GetPlain 按 ID 取完整节点(含密码,内部用途)
func GetPlain(id string) (Node, bool) {
	lock.Lock()
	defer lock.Unlock()
	for _, n := range loadLocked().Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// Add 新增节点(host+user+password 必填;指纹可空,首连检查时回填)
func Add(n Node) (Node, error) {
	n = normalize(n)
	if err := validate(n, true); err != nil {
		return Node{}, err
	}
	lock.Lock()
	defer lock.Unlock()
	st := loadLocked()
	for _, o := range st.Nodes {
		if o.Host == n.Host && o.Port == n.Port {
			return Node{}, fmt.Errorf("该主机端口已存在节点「%s」", o.Name)
		}
	}
	n.ID = fmt.Sprintf("nd_%d", time.Now().UnixNano())
	n.CreatedAt = time.Now().Format(time.RFC3339)
	st.Nodes = append(st.Nodes, n)
	if err := saveLocked(st); err != nil {
		return Node{}, err
	}
	n.Password = ""
	n.PasswordSet = true
	return n, nil
}

// Update 编辑节点(密码为空 = 沿用;指纹变更需显式提供新值)
func Update(id string, n Node) (Node, error) {
	n = normalize(n)
	lock.Lock()
	defer lock.Unlock()
	st := loadLocked()
	for i, o := range st.Nodes {
		if o.ID != id {
			continue
		}
		if n.Name == "" || n.Host == "" || n.User == "" {
			return Node{}, fmt.Errorf("名称、主机、用户名不能为空")
		}
		if n.Password == "" {
			n.Password = o.Password
		}
		if n.Fingerprint == "" {
			n.Fingerprint = o.Fingerprint
		}
		n.ID, n.CreatedAt, n.PasswordSet = id, o.CreatedAt, n.Password != ""
		st.Nodes[i] = n
		if err := saveLocked(st); err != nil {
			return Node{}, err
		}
		n.Password = ""
		return n, nil
	}
	return Node{}, fmt.Errorf("节点不存在")
}

// Delete 删除节点
func Delete(id string) error {
	lock.Lock()
	defer lock.Unlock()
	st := loadLocked()
	out := st.Nodes[:0]
	found := false
	for _, n := range st.Nodes {
		if n.ID == id {
			found = true
			continue
		}
		out = append(out, n)
	}
	if !found {
		return fmt.Errorf("节点不存在")
	}
	st.Nodes = out
	return saveLocked(st)
}

func normalize(n Node) Node {
	n.Name = strings.TrimSpace(n.Name)
	n.Group = strings.TrimSpace(n.Group)
	n.Host = strings.TrimSpace(n.Host)
	n.User = strings.TrimSpace(n.User)
	n.Fingerprint = strings.TrimSpace(n.Fingerprint)
	if n.Port <= 0 {
		n.Port = 22
	}
	return n
}

func validate(n Node, withPassword bool) error {
	if n.Name == "" || n.Host == "" || n.User == "" {
		return fmt.Errorf("名称、主机、用户名不能为空")
	}
	if withPassword && n.Password == "" {
		return fmt.Errorf("密码不能为空")
	}
	if n.Port <= 0 || n.Port > 65535 {
		return fmt.Errorf("SSH 端口不合法")
	}
	if len(n.Name) > 64 || len(n.Group) > 32 || len(n.Desc) > 200 {
		return fmt.Errorf("名称/分组/备注超长")
	}
	return nil
}

// ---- SSH 通道 ----

// dial SSH 连接;指纹已配置时强校验,未配置时 TOFU 接受并回显实际指纹。
// 返回 (client, 实际指纹, err)
func dial(n Node) (*ssh.Client, string, error) {
	var actual string
	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		actual = ssh.FingerprintSHA256(key)
		if n.Fingerprint != "" && actual != n.Fingerprint {
			return fmt.Errorf("主机指纹不匹配: 期望 %s,实际 %s(连接可能被劫持,已中止;若确认是服务器重装所致,请在编辑中更新指纹)", n.Fingerprint, actual)
		}
		return nil
	}
	cfg := &ssh.ClientConfig{
		User:            n.User,
		Auth:            []ssh.AuthMethod{ssh.Password(n.Password)},
		HostKeyCallback: cb,
		Timeout:         12 * time.Second,
	}
	cl, err := ssh.Dial("tcp", net.JoinHostPort(n.Host, strconv.Itoa(n.Port)), cfg)
	if err != nil {
		if actual != "" && n.Fingerprint == "" {
			return nil, actual, fmt.Errorf("SSH 认证/连接失败: %v", err)
		}
		return nil, actual, err
	}
	return cl, actual, nil
}

// run 远程命令(stdout+stderr 合并,带超时)
func run(cl *ssh.Client, timeout time.Duration, cmd string) (string, error) {
	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var buf strings.Builder
	sess.Stdout = &buf
	sess.Stderr = &buf
	done := make(chan error, 1)
	if err := sess.Start(cmd); err != nil {
		return "", err
	}
	go func() { done <- sess.Wait() }()
	select {
	case <-time.After(timeout):
		_ = sess.Signal(ssh.SIGKILL)
		<-done
		return buf.String(), fmt.Errorf("远程命令超时")
	case err := <-done:
		if err != nil {
			tail := buf.String()
			if len(tail) > 200 {
				tail = tail[len(tail)-200:]
			}
			return buf.String(), fmt.Errorf("%v: %s", err, tail)
		}
		return buf.String(), nil
	}
}

// probeScript 固定采集脚本:双采样 CPU/网络 + 内存/磁盘/系统信息 + 面板探测。
// 无任何外部输入拼接,base64 下发避免引号转义问题。
const probeScript = `cpu_a=$(awk '/^cpu /{print $2+$3+$4+$6+$7+$8" "$5" "$2+$3+$4+$6+$7+$8+$5}' /proc/stat)
rx_a=0; tx_a=0
while read -r ifc rxb _ _ _ _ _ _ _ txb _; do
  case "$ifc" in *:) ifc=${ifc%:} ;; *) continue ;; esac
  [ "$ifc" = "lo" ] && continue
  rx_a=$((rx_a+rxb)); tx_a=$((tx_a+txb))
done < /proc/net/dev
sleep 1
awk -v a="$cpu_a" -v b="$(awk '/^cpu /{print $2+$3+$4+$6+$7+$8" "$5" "$2+$3+$4+$6+$7+$8+$5}' /proc/stat)" 'BEGIN{split(a,x," ");split(b,y," ");db=y[1]-x[1];di=y[2]-x[2];dt=y[3]-x[3];if(dt>0)printf "cpu=%.1f\n",(dt-di)*100/dt;else print "cpu=0"}'
while read -r ifc rxb _ _ _ _ _ _ _ txb _; do
  case "$ifc" in *:) ifc=${ifc%:} ;; *) continue ;; esac
  [ "$ifc" = "lo" ] && continue
  rx_b=$((rx_b+rxb)); tx_b=$((tx_b+txb))
done < /proc/net/dev
awk -v a="$rx_a $tx_a" -v b="$rx_b $tx_b" 'BEGIN{split(a,x," ");split(b,y," ");printf "net_rx=%.0f\nnet_tx=%.0f\n",(y[1]-x[1]),(y[2]-x[2])}'
free -b | awk '/^Mem:/{printf "mem_total=%s\nmem_used=%s\n",$2,$3} /^Swap:/{if($2>0)printf "swap_total=%s\nswap_used=%s\n",$2,$3}'
df -kP | awk '$1 ~ /^\// && !seen[$6]++ {t+=$2;u+=$3} END{printf "disk_total=%.0f\ndisk_used=%.0f\n",t*1024,u*1024}'
awk '{printf "uptime=%d\n",$1}' /proc/uptime
read l1 _ < /proc/loadavg; echo "load1=$l1"
echo "cores=$(nproc)"
echo "kernel=$(uname -r)"
echo "hostname=$(hostname 2>/dev/null)"
. /etc/os-release 2>/dev/null && echo "os=${NAME} ${VERSION_ID}"
echo "---kunpanel---"
if systemctl is-active kunpanel >/dev/null 2>&1; then
  echo "panel_active=1"
  if [ -r /opt/kunpanel/data/config.json ]; then
    grep -o '"port"[^,}]*' /opt/kunpanel/data/config.json | head -1
    grep -o '"entrance"[^,}]*' /opt/kunpanel/data/config.json | head -1
    grep -o '"ssl"[^,}]*' /opt/kunpanel/data/config.json | head -1
  fi
fi`

// Probe 连接节点采集快照(不写缓存)
func Probe(n Node) Snapshot {
	s := Snapshot{CheckedAt: time.Now().Format(time.RFC3339)}
	cl, _, err := dial(n)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	defer cl.Close()
	out, err := run(cl, 20*time.Second, "echo "+base64.StdEncoding.EncodeToString([]byte(probeScript))+" | base64 -d | sh")
	if err != nil {
		s.Error = err.Error()
		return s
	}
	parseSnapshot(&s, out)
	s.Online = s.Error == ""
	return s
}

// parseSnapshot 解析 probeScript 输出(单测覆盖)
func parseSnapshot(s *Snapshot, out string) {
	inPanel := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "---kunpanel---" {
			inPanel = true
			continue
		}
		var k, v string
		if inPanel {
			// 面板探测段输出两种格式:shell 的 k=v 与 grep config.json 的 "k": v
			line = strings.TrimSuffix(strings.TrimSpace(line), ",")
			if strings.HasPrefix(line, "\"") {
				i := strings.Index(line, "\":")
				if i < 0 {
					continue
				}
				k = strings.Trim(line[:i+1], "\"")
				v = strings.TrimSpace(line[i+2:])
			} else {
				var ok bool
				k, v, ok = cutKV(line)
				if !ok {
					continue
				}
			}
		} else {
			var ok bool
			k, v, ok = cutKV(line)
			if !ok {
				continue
			}
		}
		switch {
		case inPanel:
			switch k {
			case "panel_active":
				s.PanelInstalled = v == "1"
			case "port":
				if p, e := strconv.Atoi(strings.Trim(strings.TrimPrefix(v, ":"), "\" ")); e == nil {
					s.PanelPort = p
				}
			case "entrance":
				s.PanelEntrance = strings.Trim(v, "\" ")
			case "ssl":
				s.PanelSSL = strings.Contains(v, "true")
			}
		default:
			switch k {
			case "cpu":
				s.CPUPercent, _ = strconv.ParseFloat(v, 64)
			case "net_rx":
				s.NetRx, _ = strconv.ParseFloat(v, 64)
			case "net_tx":
				s.NetTx, _ = strconv.ParseFloat(v, 64)
			case "mem_total":
				s.MemTotal, _ = strconv.ParseUint(v, 10, 64)
			case "mem_used":
				s.MemUsed, _ = strconv.ParseUint(v, 10, 64)
			case "swap_total":
				s.SwapTotal, _ = strconv.ParseUint(v, 10, 64)
			case "swap_used":
				s.SwapUsed, _ = strconv.ParseUint(v, 10, 64)
			case "disk_total":
				s.DiskTotal, _ = strconv.ParseUint(v, 10, 64)
			case "disk_used":
				s.DiskUsed, _ = strconv.ParseUint(v, 10, 64)
			case "uptime":
				s.UptimeSec, _ = strconv.ParseUint(v, 10, 64)
			case "load1":
				s.Load1, _ = strconv.ParseFloat(v, 64)
			case "cores":
				s.Cores, _ = strconv.Atoi(v)
			case "kernel":
				s.Kernel = v
			case "hostname":
				s.Hostname = v
			case "os":
				s.OS = v
			}
		}
	}
}

func cutKV(line string) (string, string, bool) {
	i := strings.IndexByte(line, '=')
	if i <= 0 {
		return "", "", false
	}
	return line[:i], line[i+1:], true
}

// ---- 缓存:列表接口并发采集,SWR 20s ----

type cacheEntry struct {
	snaps map[string]Snapshot
	at    time.Time
}

var (
	snapCache   cacheEntry
	snapCacheMu sync.Mutex
	fetching    bool
)

// Snapshots 节点指标快照(缓存 20s)。
// 热缓存直接返回;force(手动刷新)或冷启动(首采)同步并发采集——
// 否则刚添加的节点/首次打开页面会全部显示"离线";仅缓存过期的常规轮询
// 先返旧值并后台刷新。
func Snapshots(force bool) map[string]Snapshot {
	snapCacheMu.Lock()
	fresh := snapCache.snaps != nil && time.Since(snapCache.at) < 20*time.Second
	if fresh && !force {
		// 缓存里缺新添加节点的快照 → 视同手动刷新立即补采,
		// 否则新节点要等缓存过期(最长 20s)才从"离线"变在线
		for _, n := range listPlain() {
			if _, ok := snapCache.snaps[n.ID]; !ok {
				force = true
				break
			}
		}
	}
	if !force && fresh && fetching {
		m := snapCache.snaps
		snapCacheMu.Unlock()
		return m
	}
	syncFetch := force || snapCache.snaps == nil
	fetching = true
	nodesList := listPlain() // 采集必须用完整凭据(List 是脱敏视图)
	old := snapCache.snaps
	snapCacheMu.Unlock()

	fill := func() map[string]Snapshot {
		var wg sync.WaitGroup
		var mu2 sync.Mutex
		snaps := make(map[string]Snapshot, len(nodesList))
		for _, n := range nodesList {
			wg.Add(1)
			go func(n Node) {
				defer wg.Done()
				s := Probe(n)
				mu2.Lock()
				snaps[n.ID] = s
				mu2.Unlock()
			}(n)
		}
		wg.Wait()
		snapCacheMu.Lock()
		fetching = false
		// 全部失败且有旧缓存时保留旧值,避免网络抖动把面板清空
		allFail := len(snaps) > 0
		for _, s := range snaps {
			if s.Online {
				allFail = false
				break
			}
		}
		if allFail && old != nil {
			m := old
			snapCacheMu.Unlock()
			return m
		}
		snapCache.snaps = snaps
		snapCache.at = time.Now()
		snapCacheMu.Unlock()
		return snaps
	}

	if syncFetch {
		return fill()
	}
	go fill()
	snapCacheMu.Lock()
	m := snapCache.snaps
	snapCacheMu.Unlock()
	if m == nil {
		m = map[string]Snapshot{}
	}
	return m
}

// Check 可用性检查(添加/编辑弹窗用):SSH 连通+认证,顺带采集与面板探测。
// 未提供指纹时返回实际指纹供用户确认入库(TOFU)。
func Check(n Node) (Snapshot, string, error) {
	n = normalize(n)
	if n.Host == "" || n.User == "" {
		return Snapshot{}, "", fmt.Errorf("主机与用户名不能为空")
	}
	cl, actual, err := dial(n)
	if err != nil {
		if actual != "" && n.Fingerprint == "" {
			// 认证失败但握手成功:指纹照样回显供确认
			return Snapshot{Online: false, Error: err.Error()}, actual, nil
		}
		return Snapshot{Online: false, Error: err.Error()}, actual, err
	}
	defer cl.Close()
	out, err := run(cl, 20*time.Second, "echo "+base64.StdEncoding.EncodeToString([]byte(probeScript))+" | base64 -d | sh")
	s := Snapshot{CheckedAt: time.Now().Format(time.RFC3339)}
	if err != nil {
		s.Error = err.Error()
		return s, actual, nil
	}
	parseSnapshot(&s, out)
	s.Online = true
	return s, actual, nil
}

// PanelURL 拼对端面板访问地址(仅 http/https)
func PanelURL(s Snapshot, host string) string {
	if !s.PanelInstalled || s.PanelPort <= 0 {
		return ""
	}
	scheme := "http"
	if s.PanelSSL {
		scheme = "https"
	}
	u := fmt.Sprintf("%s://%s:%d", scheme, host, s.PanelPort)
	if s.PanelEntrance != "" && s.PanelEntrance != "-" {
		u += "/" + strings.Trim(s.PanelEntrance, "/")
	}
	return u
}

// RestartPanel 重启对端面板
func RestartPanel(n Node) error {
	cl, _, err := dial(n)
	if err != nil {
		return err
	}
	defer cl.Close()
	_, err = run(cl, 30*time.Second, "systemctl restart kunpanel && sleep 1 && systemctl is-active kunpanel")
	return err
}

// Reboot 重启对端服务器(调用方负责二次确认;连接会被切断属预期)
func Reboot(n Node) error {
	cl, _, err := dial(n)
	if err != nil {
		return err
	}
	defer cl.Close()
	// detach 免得会话随重启报错;shutdown 延迟 2s 让本命令先返回
	_, _ = run(cl, 10*time.Second, "nohup sh -c 'sleep 2; shutdown -r now' >/dev/null 2>&1 & exit 0")
	return nil
}

// InstallPanel 在节点上安装坤面板(后台任务用;官方一键安装脚本)
func InstallPanel(n Node, logf func(string)) error {
	cl, _, err := dial(n)
	if err != nil {
		return err
	}
	defer cl.Close()
	script := "curl -fsSL https://raw.githubusercontent.com/kungec/kunpanel/main/install.sh | bash 2>&1; echo KUNINSTALL_EXIT=$?"
	sess, err := cl.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	if err := sess.Start(script); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	var tail string
	for {
		nread, e := stdout.Read(buf)
		if nread > 0 {
			chunk := string(buf[:nread])
			tail = (tail + chunk)
			if len(tail) > 4000 {
				tail = tail[len(tail)-4000:]
			}
			if logf != nil {
				for _, ln := range strings.Split(strings.TrimRight(chunk, "\n"), "\n") {
					if strings.TrimSpace(ln) != "" {
						logf(ln)
					}
				}
			}
		}
		if e != nil {
			break
		}
	}
	_ = sess.Wait()
	if strings.Contains(tail, "KUNINSTALL_EXIT=0") {
		return nil
	}
	return fmt.Errorf("安装脚本退出异常,请查看任务日志")
}

package nodes

import (
	"strings"
	"testing"
)

func TestParseSnapshot(t *testing.T) {
	out := `cpu=23.4
net_rx=15234
net_tx=678
mem_total=4039152128
mem_used=1234567
swap_total=1073737728
swap_used=0
disk_total=41238630400
disk_used=9876543210
uptime=864000
load1=0.35
cores=2
kernel=5.15.0-101-generic
hostname=my-node
os=Ubuntu 22.04
---kunpanel---
panel_active=1
"port": 8021,
"entrance": "kp_abc",
"ssl": false,
`
	var s Snapshot
	parseSnapshot(&s, out)
	if s.CPUPercent != 23.4 || s.NetRx != 15234 || s.NetTx != 678 {
		t.Errorf("CPU/网络解析错误: %+v", s)
	}
	if s.MemTotal != 4039152128 || s.MemUsed != 1234567 {
		t.Errorf("内存解析错误: %+v", s)
	}
	if s.DiskTotal != 41238630400 || s.DiskUsed != 9876543210 {
		t.Errorf("磁盘解析错误: %+v", s)
	}
	if s.UptimeSec != 864000 || s.Load1 != 0.35 || s.Cores != 2 {
		t.Errorf("系统信息解析错误: %+v", s)
	}
	if s.Hostname != "my-node" || s.OS != "Ubuntu 22.04" || s.Kernel != "5.15.0-101-generic" {
		t.Errorf("OS 信息解析错误: %+v", s)
	}
	if !s.PanelInstalled || s.PanelPort != 8021 || s.PanelEntrance != "kp_abc" || s.PanelSSL {
		t.Errorf("面板探测解析错误: %+v", s)
	}
}

func TestParseSnapshotNoPanel(t *testing.T) {
	var s Snapshot
	parseSnapshot(&s, "cpu=1.0\nload1=0.1\n")
	if s.PanelInstalled {
		t.Errorf("无面板输出时 PanelInstalled 应为 false")
	}
	parseSnapshot(&s, "---kunpanel---\nnothing here\n")
	if s.PanelInstalled {
		t.Errorf("面板未激活时 PanelInstalled 应为 false")
	}
}

func TestParseSnapshotEntranceClosed(t *testing.T) {
	var s Snapshot
	parseSnapshot(&s, "---kunpanel---\npanel_active=1\n\"port\": 51814,\n\"entrance\": \"-\",\n\"ssl\": true,\n")
	if !s.PanelInstalled || s.PanelPort != 51814 || !s.PanelSSL {
		t.Errorf("解析错误: %+v", s)
	}
	if u := PanelURL(s, "1.2.3.4"); u != "https://1.2.3.4:51814" {
		t.Errorf("入口关闭时 URL 不应带路径: %s", u)
	}
}

func TestPanelURL(t *testing.T) {
	s := Snapshot{PanelInstalled: true, PanelPort: 8000, PanelEntrance: "kp_x", PanelSSL: false}
	if got := PanelURL(s, "example.com"); got != "http://example.com:8000/kp_x" {
		t.Errorf("URL 错误: %s", got)
	}
	s.PanelInstalled = false
	if got := PanelURL(s, "example.com"); got != "" {
		t.Errorf("未装面板应返回空 URL: %s", got)
	}
}

func TestValidate(t *testing.T) {
	n := Node{Name: "a", Host: "1.2.3.4", User: "root", Password: "p", Port: 22}
	if err := validate(n, true); err != nil {
		t.Fatalf("合法节点报错: %v", err)
	}
	n.Password = ""
	if err := validate(n, true); err == nil {
		t.Errorf("新增缺密码应报错")
	}
	if err := validate(n, false); err != nil {
		t.Errorf("编辑沿用密码应通过: %v", err)
	}
	n.Port = 0
	if err := validate(n, false); err == nil {
		t.Errorf("端口 0 应报错")
	}
	n.Port = 70000
	if err := validate(n, false); err == nil {
		t.Errorf("端口 70000 应报错")
	}
}

func TestStoreCRUD(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	n, err := Add(Node{Name: "n1", Host: "10.0.0.9", Port: 2222, User: "root", Password: "pw", Group: "生产", Fingerprint: "SHA256:abc"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if n.Password != "" || !n.PasswordSet {
		t.Errorf("Add 返回值应脱敏且标记 password_set")
	}
	// 同主机端口去重
	if _, err := Add(Node{Name: "n2", Host: "10.0.0.9", Port: 2222, User: "root", Password: "pw"}); err == nil {
		t.Errorf("重复主机端口应报错")
	}
	// 列表脱敏 + 内部明文视图分离(Snapshots 采集必须用明文,勿回归)
	ls := List()
	if len(ls) != 1 || ls[0].Password != "" || !strings.HasPrefix(ls[0].ID, "nd_") {
		t.Fatalf("List 异常: %+v", ls)
	}
	if pl := listPlain(); len(pl) != 1 || pl[0].Password != "pw" {
		t.Fatalf("listPlain 应含密码: %+v", pl)
	}
	// 编辑:密码留空沿用;空指纹沿用
	if _, err := Update(n.ID, Node{Name: "n1b", Host: "10.0.0.9", Port: 2222, User: "root", Group: "测试"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	p, ok := GetPlain(n.ID)
	if !ok || p.Password != "pw" || p.Fingerprint != "SHA256:abc" || p.Group != "测试" {
		t.Fatalf("Update 沿用失败: %+v ok=%v", p, ok)
	}
	if err := Delete(n.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(List()) != 0 {
		t.Errorf("删除后列表应为空")
	}
	if err := Delete("nd_none"); err == nil {
		t.Errorf("删除不存在节点应报错")
	}
}

func TestProbeScriptConstants(t *testing.T) {
	// 采集脚本必须含面板探测段与双采样;脚本为固定常量,不拼接任何用户输入
	for _, frag := range []string{"---kunpanel---", "/proc/stat", "sleep 1", "/etc/os-release", "df -kP", "nproc"} {
		if !strings.Contains(probeScript, frag) {
			t.Errorf("probeScript 缺少 %s", frag)
		}
	}
	if strings.Contains(probeScript, "\"+n.") {
		t.Errorf("probeScript 不应包含拼接占位")
	}
}

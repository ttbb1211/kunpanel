package server

// 节点管理 API(对标 1Panel 专业版「节点管理」,免费内置)
//
// 路由见 api_routes.go /api/v1/nodes*。凭据仅入库存本地,列表/详情一律脱敏。

import (
	"io"
	"net/http"
	"strings"

	"kunpanel/internal/nodes"
	"kunpanel/internal/taskx"
)

// nodeView 列表视图:节点 + 快照 + 面板直达地址
type nodeView struct {
	nodes.Node
	Snap     nodes.Snapshot `json:"snap"`
	PanelURL string         `json:"panel_url"`
}

func (s *Server) handleNodeList(w http.ResponseWriter, r *http.Request) {
	list := nodes.List()
	force := r.URL.Query().Get("refresh") == "1"
	snaps := nodes.Snapshots(force)
	views := make([]nodeView, 0, len(list))
	for _, n := range list {
		snap := snaps[n.ID]
		views = append(views, nodeView{Node: n, Snap: snap, PanelURL: nodes.PanelURL(snap, n.Host)})
	}
	OK(w, map[string]any{"nodes": views})
}

func (s *Server) handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	var req nodes.Node
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	n, err := nodes.Add(req)
	if err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, n)
}

func (s *Server) handleNodeUpdate(w http.ResponseWriter, r *http.Request) {
	var req nodes.Node
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	n, err := nodes.Update(r.PathValue("id"), req)
	if err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, n)
}

func (s *Server) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	if err := nodes.Delete(r.PathValue("id")); err != nil {
		Fail(w, http.StatusNotFound, err.Error())
		return
	}
	OK(w, nil)
}

// handleNodeCheck 可用性检查(添加/编辑弹窗,不入库):SSH 连通+认证+系统信息+面板探测,
// 未带指纹时回显实际指纹供 TOFU 确认。
func (s *Server) handleNodeCheck(w http.ResponseWriter, r *http.Request) {
	var req nodes.Node
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	// 编辑场景密码留空 = 沿用已存密码
	if req.Password == "" && req.ID != "" {
		if old, ok := nodes.GetPlain(req.ID); ok {
			req.Password = old.Password
			if req.Fingerprint == "" {
				req.Fingerprint = old.Fingerprint
			}
		}
	}
	snap, fp, err := nodes.Check(req)
	OK(w, map[string]any{
		"snap":        snap,
		"fingerprint": fp,
		"err":         errString(err),
	})
}

// handleNodeAction 重启面板 / 重启服务器 / 安装面板(后台任务)
func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.PathValue("action")
	n, ok := nodes.GetPlain(id)
	if !ok {
		Fail(w, http.StatusNotFound, "节点不存在")
		return
	}
	switch action {
	case "restart-panel":
		if err := nodes.RestartPanel(n); err != nil {
			Fail(w, http.StatusInternalServerError, "重启面板失败: "+err.Error())
			return
		}
		OK(w, nil)
	case "reboot":
		if err := nodes.Reboot(n); err != nil {
			Fail(w, http.StatusInternalServerError, "重启指令下发失败: "+err.Error())
			return
		}
		OK(w, nil)
	case "install-panel":
		if _, err := taskx.M.Start("节点安装面板-"+n.Name, func(w io.Writer) error {
			lw := &lineWriter{w: w}
			if err := nodes.InstallPanel(n, lw.line); err != nil {
				return err
			}
			_, _ = w.Write([]byte("安装完成,正在刷新节点状态\n"))
			nodes.Snapshots(true)
			return nil
		}); err != nil {
			Fail(w, http.StatusConflict, err.Error())
			return
		}
		OK(w, nil)
	default:
		Fail(w, http.StatusBadRequest, "未知操作")
	}
}

// lineWriter 把整块输出拆成行喂给任务日志
type lineWriter struct{ w io.Writer }

func (lw *lineWriter) line(s string) {
	_, _ = io.WriteString(lw.w, strings.TrimRight(s, "\n")+"\n")
}

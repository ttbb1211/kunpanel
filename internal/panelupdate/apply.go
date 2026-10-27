// 面板在线更新:下载校验后的安装、替换与重启
package panelupdate

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"kunpanel/internal/util"
)

// Apply 从任一可用更新源下载指定版本,校验后替换本机二进制并触发服务重启。
// 重启通过脱离的子进程执行,面板进程先正常返回 HTTP 响应再退出。
func Apply(tag string, w io.Writer) error {
	tag = strings.TrimSpace(tag)
	if _, ok := Semver(tag); !ok {
		return fmt.Errorf("非法版本号: %s", tag)
	}
	asset := assetName(tag)
	archive := filepath.Join(os.TempDir(), fmt.Sprintf("kunpanel_update_%d%s", time.Now().UnixNano(), map[bool]string{true: ".zip", false: ".tar.gz"}[runtime.GOOS == "windows"]))
	defer os.Remove(archive)

	var lastErr error
	for _, src := range sources {
		fmt.Fprintf(w, "更新源 %s:下载 %s ...\n", src.name, tag)
		if err := src.download(tag, asset, archive, w); err != nil {
			fmt.Fprintf(w, "源 %s 下载失败: %v,尝试下一源 ...\n", src.name, err)
			lastErr = err
			continue
		}
		// 完整性校验:与同一来源的 SHA256SUMS 比对,发布链路被篡改时中止
		fmt.Fprintf(w, "校验安装包完整性(SHA-256,来源 %s)...\n", src.name)
		sums, err := src.sums(tag)
		if err != nil {
			fmt.Fprintf(w, "源 %s 获取校验和失败: %v,尝试下一源 ...\n", src.name, err)
			lastErr = err
			continue
		}
		want := expectedHash(sums, asset)
		if want == "" {
			return fmt.Errorf("官方校验和中没有 %s 的记录,已中止升级", asset)
		}
		got, err := fileHash(archive)
		if err != nil {
			return fmt.Errorf("计算安装包哈希失败: %v", err)
		}
		if got != want {
			return fmt.Errorf("安装包校验失败(期望 %s... 实际 %s...),已中止升级:发布链路可能被篡改", want[:12], got[:12])
		}
		fmt.Fprintln(w, "校验通过(来源 "+src.name+")")
		return installFrom(archive, w)
	}
	return fmt.Errorf("三个更新源均下载失败: %v", lastErr)
}

// installFrom 解压并替换本机二进制,触发重启
func installFrom(archive string, w io.Writer) error {
	workdir := filepath.Join(os.TempDir(), fmt.Sprintf("kunpanel_update_%d", time.Now().UnixNano()))
	_ = os.MkdirAll(workdir, 0o755)
	defer os.RemoveAll(workdir)

	fmt.Fprintln(w, "解压 ...")
	if runtime.GOOS == "windows" {
		if err := unzip(archive, workdir); err != nil {
			return fmt.Errorf("解压失败: %v", err)
		}
	} else {
		if out, err := util.Run("tar", "-xzf", archive, "-C", workdir); err != nil {
			return fmt.Errorf("解压失败: %v %s", err, strings.TrimSpace(out))
		}
	}

	// 在解压目录里找 kunpanel[.exe]
	var newPath string
	binName := "kunpanel"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	filepath.Walk(workdir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && filepath.Base(p) == binName {
			newPath = p
			return filepath.SkipAll
		}
		return nil
	})
	if newPath == "" {
		return fmt.Errorf("安装包内未找到 %s", binName)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身失败: %v", err)
	}
	self, _ = filepath.EvalSymlinks(self)

	if runtime.GOOS == "windows" {
		// Windows 运行中的 exe 不能覆盖但可以改名:旧→.old,新→原名
		old := self + ".old"
		_ = os.Remove(old)
		if err := os.Rename(self, old); err != nil {
			return fmt.Errorf("备份旧程序失败: %v", err)
		}
		if err := copyFile(newPath, self); err != nil {
			_ = os.Rename(old, self) // 回滚
			return fmt.Errorf("写入新程序失败: %v", err)
		}
	} else {
		tmp := self + ".new"
		if err := copyFile(newPath, tmp); err != nil {
			return fmt.Errorf("写入新程序失败: %v", err)
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			return fmt.Errorf("chmod 失败: %v", err)
		}
		// 原子替换:旧版保留为 .bak 供回滚
		bak := self + ".bak"
		_ = os.Remove(bak)
		_ = os.Rename(self, bak)
		if err := os.Rename(tmp, self); err != nil {
			_ = os.Rename(bak, self)
			return fmt.Errorf("替换失败: %v", err)
		}
	}
	fmt.Fprintln(w, "已替换程序,即将重启面板服务 ...")
	restartSelf(w)
	return nil
}

// restartSelf 脱离子进程执行重启(面板进程先返回响应再被重启)。
// 字面量参数直连服务管理器,无 shell 参与;安装脚本已注册 systemd/OpenRC 服务,
// 极少数无 init 环境由用户手动重启(有输出提示)。
func restartSelf(w io.Writer) {
	if runtime.GOOS == "windows" {
		c1 := exec.Command("sc", "stop", "kunpanel")
		c1.SysProcAttr = detached()
		_ = c1.Start()
		time.Sleep(3 * time.Second)
		c2 := exec.Command("sc", "start", "kunpanel")
		c2.SysProcAttr = detached()
		_ = c2.Start()
		return
	}
	c := exec.Command("systemctl", "restart", "kunpanel")
	c.SysProcAttr = detached()
	if err := c.Start(); err == nil {
		return
	}
	c = exec.Command("rc-service", "kunpanel", "restart")
	c.SysProcAttr = detached()
	if err := c.Start(); err == nil {
		return
	}
	fmt.Fprintln(w, "未检测到 systemd/OpenRC,请手动重启面板服务")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func unzip(archive, dir string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		p := filepath.Join(dir, f.Name)
		if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(dir)+string(os.PathSeparator)) {
			continue // 防 ZipSlip
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(p, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		src, err := f.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

package app

import (
	"os/exec"
	"strings"
	"testing"
)

func TestChineseCommandHelp(t *testing.T) {
	binary, cleanup := buildWebhook(t)
	defer cleanup()
	for _, test := range []struct {
		command string
		want    []string
	}{
		{"", []string{"配置文件：", "-config", "Webhook 服务监听的端口", "默认值：9000"}},
		{"hooks", []string{"本地磁盘配置"}},
		{"config", []string{"最小本地配置"}},
		{"service", []string{"原生管理", "install", "status"}},
		{"service install", []string{"不调用辅助脚本", "dry-run", "working-directory"}},
		{"service status", []string{"不自动提权", "timeout"}},
		{"service uninstall", []string{"不自动提权"}},
		{"config init", []string{"拒绝覆盖", "dry-run", "profile", "format"}},
		{"config template", []string{"带中文注释", "list"}},
		{"config template hook", []string{"拒绝覆盖", "format", "output"}},
		{"hooks reload", []string{"加载回执", "runtime-dir", "instance"}},
		{"hooks list", []string{"本地 Hook 配置", "-config", "-working-directory"}},
		{"hooks show", []string{"Hook ID", "-show-secrets"}},
		{"hooks validate", []string{"本地 Hook 配置"}},
		{"hooks explain", []string{"直接指定本地上下文", "remote-ip", "uploaded-file"}},
		{"hooks test", []string{"直接指定本地上下文", "execute", "strict"}},
		{"hooks add", []string{"保存前备份", "dry-run", "reload", "from"}},
		{"hooks update", []string{"保存前备份", "from"}},
		{"hooks delete", []string{"保存前备份", "id"}},
		{"agent", []string{"Hub 基础地址", "自定义反向代理路径前缀", "保存节点令牌", "默认值：1h0m0s"}},
		{"update", []string{"检查可用更新", "下载并应用更新", "恢复上一版本"}},
		{"update check", []string{"发布版本号", "JSON 格式", "默认值："}},
		{"update apply", []string{"交互确认", "可执行文件路径", "更新状态目录"}},
		{"update rollback", []string{"恢复上一版本", "可执行文件路径", "更新状态目录"}},
	} {
		for _, help := range []string{"-h", "--help"} {
			t.Run(test.command+"/"+help, func(t *testing.T) {
				args := append(strings.Fields(test.command), help)
				output, err := exec.Command(binary, args...).CombinedOutput()
				if err != nil {
					t.Fatalf("help failed: %v\n%s", err, output)
				}
				text := string(output)
				for _, want := range append([]string{"用法："}, test.want...) {
					if !strings.Contains(text, want) {
						t.Errorf("help missing %q:\n%s", want, text)
					}
				}
				for _, unwanted := range []string{"Usage:", "Usage of", "(default ", " string\n", " duration\n"} {
					if strings.Contains(text, unwanted) {
						t.Errorf("help contains untranslated text %q", unwanted)
					}
				}
			})
		}
	}
}

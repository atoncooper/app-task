package cli

// at channel: manage notify channels (dingtalk/feishu/webhook/email groups)
// that notify tasks reference via payload "channel". Config values are
// write-only server-side; the CLI never displays them back.

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newChannelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "通知渠道管理（notify 任务投递目标：钉钉/飞书/Teams/webhook/邮件组）",
	}
	cmd.AddCommand(
		newChannelLsCmd(),
		newChannelAddCmd(),
		newChannelTestCmd(),
		newChannelRmCmd(),
	)
	return cmd
}

func newChannelLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "列出渠道",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Items []struct {
					Name      string `json:"name"`
					Type      string `json:"type"`
					Enabled   bool   `json:"enabled"`
					UpdatedAt string `json:"updated_at"`
				} `json:"items"`
				Total int `json:"total"`
			}
			if err := c.get("/api/channels", &out); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tSTATUS\tUPDATED_AT")
			for _, ch := range out.Items {
				status := "enabled"
				if !ch.Enabled {
					status = "disabled"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ch.Name, ch.Type, status, ch.UpdatedAt)
			}
			fmt.Fprintf(w, "—— 共 %d 条\n", out.Total)
			return w.Flush()
		},
	}
}

func newChannelAddCmd() *cobra.Command {
	var (
		name   string
		chType string
		config string
	)
	cmd := &cobra.Command{
		Use:   "add --name <name> --type <type> --config '<json>'",
		Short: "新建/更新渠道（同名即覆盖配置）",
		Example: `  at channel add --name ops-ding --type dingtalk \
    --config '{"webhook":"https://oapi.dingtalk.com/robot/send?access_token=x","sign_secret":"SEC..."}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Name    string `json:"name"`
				Created bool   `json:"created"`
			}
			if err := c.post("/api/channels", map[string]string{
				"name": name, "type": chType, "config": config,
			}, &out); err != nil {
				return err
			}
			if out.Created {
				fmt.Printf("渠道已创建：%s（%s）\n", out.Name, chType)
			} else {
				fmt.Printf("渠道配置已更新：%s（%s）\n", out.Name, chType)
			}
			fmt.Println("用 at channel test 验证连通后，在 notify 任务 payload 里以 {\"channel\":\"" + name + "\"} 引用。")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "渠道名称（小写 [a-z0-9_-]，2–64 位）")
	cmd.Flags().StringVar(&chType, "type", "", "email | dingtalk | feishu | webhook | teams")
	cmd.Flags().StringVar(&config, "config", "", "config JSON（按类型：webhook/加签密钥/收件人/报文模板）")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("type")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func newChannelTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Args:  cobra.ExactArgs(1),
		Short: "向渠道发送一条测试消息",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Sent bool `json:"sent"`
			}
			if err := c.post("/api/channels/"+args[0]+"/test", nil, &out); err != nil {
				return err
			}
			fmt.Println("测试消息已发送")
			return nil
		},
	}
}

func newChannelRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Args:  cobra.ExactArgs(1),
		Short: "删除渠道（引用它的 notify 任务将发送失败）",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Deleted bool `json:"deleted"`
			}
			if err := c.do("DELETE", "/api/channels/"+strings.TrimSpace(args[0]), nil, &out); err != nil {
				return err
			}
			fmt.Println("已删除")
			return nil
		},
	}
}

package cli

// at calendar: business calendar management — holiday (off) / adjusted
// workday (work) date overrides applied when cron tasks materialize their
// next occurrence.

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newCalendarCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calendar",
		Short: "业务日历（cron 跳过节假日 off / 调休补班 work）",
	}
	cmd.AddCommand(
		newCalendarLsCmd(),
		newCalendarAddCmd(),
		newCalendarRmCmd(),
		newCalendarDateCmd(),
	)
	return cmd
}

func newCalendarLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "列出日历",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Items []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					Dates       int64  `json:"dates"`
				} `json:"items"`
				Total int `json:"total"`
			}
			if err := c.get("/api/calendars", &out); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDATES\tDESCRIPTION")
			for _, ca := range out.Items {
				fmt.Fprintf(w, "%s\t%d\t%s\n", ca.Name, ca.Dates, ca.Description)
			}
			fmt.Fprintf(w, "—— 共 %d 个\n", out.Total)
			return w.Flush()
		},
	}
}

func newCalendarAddCmd() *cobra.Command {
	var (
		addName     string
		description string
	)
	cmd := &cobra.Command{
		Use:   "add --name <name> [--description <text>]",
		Short: "新建日历",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Name    string `json:"name"`
				Created bool   `json:"created"`
			}
			if err := c.post("/api/calendars", map[string]string{
				"name": addName, "description": description,
			}, &out); err != nil {
				return err
			}
			fmt.Printf("日历已创建：%s（at calendar date 维护日期；任务创建时 --calendar 引用）\n", out.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&addName, "name", "", "日历名称（小写 [a-z0-9_-]，2–64 位）")
	cmd.Flags().StringVar(&description, "description", "", "说明")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newCalendarRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Args:  cobra.ExactArgs(1),
		Short: "删除日历（引用它的 cron 任务将停止扩展）",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Deleted bool `json:"deleted"`
			}
			if err := c.do("DELETE", "/api/calendars/"+strings.TrimSpace(args[0]), nil, &out); err != nil {
				return err
			}
			fmt.Println("已删除")
			return nil
		},
	}
}

func newCalendarDateCmd() *cobra.Command {
	var (
		name    string
		dateStr string
		dayType string
	)
	cmd := &cobra.Command{
		Use:   "date --calendar <name> --date 2026-10-01 --type off|work",
		Short: "设置一个日期的覆盖类型（off=休跳过 / work=调休补班）",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var out struct {
				Name    string `json:"name"`
				Date    string `json:"date"`
				DayType string `json:"day_type"`
			}
			if err := c.post("/api/calendars/dates", map[string]string{
				"name": name, "date": dateStr, "day_type": dayType,
			}, &out); err != nil {
				return err
			}
			fmt.Printf("已设置 %s = %s（%s）\n", out.Date, out.DayType, out.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "calendar", "", "日历名称")
	cmd.Flags().StringVar(&dateStr, "date", "", "日期（2006-01-02）")
	cmd.Flags().StringVar(&dayType, "type", "", "off | work")
	_ = cmd.MarkFlagRequired("calendar")
	_ = cmd.MarkFlagRequired("date")
	_ = cmd.MarkFlagRequired("type")
	return cmd
}

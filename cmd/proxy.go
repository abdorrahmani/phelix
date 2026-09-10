package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var proxyForeground bool

// ProxyCmd starts the long-running reverse-proxy daemon. By default it
// detaches into the background so the CLI returns immediately. Pass
// --foreground to keep it attached (used by the detached child and by
// process supervisors).
//
// The daemon hosts one httputil.ReverseProxy per enrolled app on that app's
// stable public port, and listens on a unix control socket so that
// 'phelix rebuild --blue-green' can atomically switch an app's target during
// a zero-downtime cut-over.
var ProxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Run the zero-downtime reverse-proxy daemon (background by default)",
	Long: `Run the reverse-proxy daemon used for blue-green and rolling deploys.

It binds each enrolled app's public port and routes traffic to whichever
internal instance is currently active. The control socket at
~/.phelix/proxy.sock lets 'phelix rebuild --blue-green' switch targets
atomically without dropping connections.

By default the daemon starts in the background and the CLI exits. Use
--foreground to run attached, 'phelix proxy status' to inspect routing,
and 'phelix proxy stop' to shut it down.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if proxyForeground {
			return runProxyForeground()
		}
		return startProxyBackground()
	},
}

var proxyStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show proxy daemon status and enrolled app routing",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		socket, err := proxy.DefaultSocketPath()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeProxy, "could not determine proxy socket path", err)
		}
		client := proxy.NewClient(socket)
		if err := client.Ping(context.Background()); err != nil {
			fmt.Printf("%s Proxy daemon is not running\n", color.YellowString("⚠"))
			fmt.Printf("  Start it with: %s\n", color.CyanString("phelix proxy"))
			return nil
		}

		statuses, err := client.Status(context.Background(), "")
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeProxy, "failed to query proxy status", err)
		}

		fmt.Printf("%s Proxy daemon is running\n", color.GreenString("✓"))
		fmt.Printf("  Control socket: %s\n", color.CyanString(socket))
		if len(statuses) == 0 {
			fmt.Println("  No apps enrolled yet.")
			return nil
		}
		fmt.Println()
		for _, s := range statuses {
			backends := make([]string, 0, len(s.Backends))
			for _, b := range s.Backends {
				entry := fmt.Sprintf("%s(%s)", b.Label, b.Host)
				if b.Weight > 0 {
					entry += fmt.Sprintf(" %d%%", b.Weight)
				}
				backends = append(backends, entry)
			}
			primary := fmt.Sprintf("%s(%s)", s.Primary.Label, s.Primary.Host)
			if s.Primary.Weight > 0 {
				primary += fmt.Sprintf(" %d%%", s.Primary.Weight)
			}
			fmt.Printf("  %s  public=:%d  primary=%s  backends=%v  in-flight=%d\n",
				color.CyanString(s.AppName),
				s.PublicPort,
				primary,
				backends,
				s.InFlight,
			)
		}
		return nil
	},
}

var proxyStopCmd = &cobra.Command{
	Use:           "stop",
	Short:         "Stop the background proxy daemon",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		socket, err := proxy.DefaultSocketPath()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeProxy, "could not determine proxy socket path", err)
		}
		client := proxy.NewClient(socket)
		if !client.IsRunning() {
			fmt.Printf("%s Proxy daemon is not running\n", color.YellowString("⚠"))
			return nil
		}
		if err := client.Shutdown(context.Background()); err != nil {
			return phelixerr.Wrap(phelixerr.CodeProxy, "failed to stop proxy", err)
		}
		fmt.Printf("%s Proxy daemon stopped\n", color.GreenString("✓"))
		return nil
	},
}

func init() {
	ProxyCmd.Flags().BoolVar(&proxyForeground, "foreground", false, "Run the proxy in the foreground instead of detaching")
	ProxyCmd.AddCommand(proxyStatusCmd)
	ProxyCmd.AddCommand(proxyStopCmd)
}

// startProxyBackground forks a detached `phelix proxy --foreground` child and
// waits until the control socket answers, then returns so the CLI can exit.
func startProxyBackground() error {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeProxy, "could not determine proxy socket path", err)
	}
	client := proxy.NewClient(socket)
	if client.IsRunning() {
		fmt.Printf("%s Proxy daemon is already running\n", color.GreenString("✓"))
		fmt.Printf("  Control socket: %s\n", color.CyanString(socket))
		fmt.Printf("  Inspect with: %s\n", color.YellowString("phelix proxy status"))
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "proxy", "--foreground")
	cmd.SysProcAttr = proxy.DetachedSysProcAttr()
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin = devnull
		cmd.Stdout = devnull
		cmd.Stderr = devnull
	}
	if err := cmd.Start(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProxy, "failed to start proxy daemon", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client.IsRunning() {
			fmt.Printf("%s Proxy daemon started in background\n", color.GreenString("✓"))
			fmt.Printf("  Control socket: %s\n", color.CyanString(socket))
			fmt.Printf("  Status: %s   Stop: %s\n",
				color.YellowString("phelix proxy status"),
				color.YellowString("phelix proxy stop"))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return phelixerr.Newf(
		phelixerr.CodeTimeout,
		"proxy daemon did not become reachable on %s within 5s",
		socket,
	)
}

// runProxyForeground is the long-running daemon loop. It is what the detached
// child (and users who pass --foreground) actually execute.
func runProxyForeground() error {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeProxy, "could not determine proxy socket path", err)
	}

	// Refuse a second foreground instance if one is already answering.
	existing := proxy.NewClient(socket)
	if existing.IsRunning() {
		return phelixerr.Newf(phelixerr.CodeProxy, "proxy daemon already running on %s", socket)
	}

	daemon := proxy.NewDaemon(socket)
	if err := daemon.Run(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProxy, "proxy daemon failed to start", err)
	}

	fmt.Printf("%s Proxy daemon listening on %s\n", color.GreenString("✓"), color.CyanString(socket))
	fmt.Printf("  Control socket: %s\n", socket)
	fmt.Printf("  Use %s to deploy apps zero-downtime.\n", color.YellowString("phelix rebuild <app> --blue-green"))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Printf("\n%s Shutting down proxy daemon (draining up to 30s)...\n", color.BlueString("→"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := daemon.Shutdown(ctx); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProxy, "proxy shutdown error", err)
	}
	fmt.Printf("%s Proxy daemon stopped\n", color.GreenString("✓"))
	return nil
}

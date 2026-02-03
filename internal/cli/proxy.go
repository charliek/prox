package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/proxy/certs"
	"github.com/charliek/prox/internal/proxy/hosts"
	"github.com/spf13/cobra"
)

// Certs command flags
var certsRegenerate bool

// certsCmd represents the certs command
var certsCmd = &cobra.Command{
	Use:   "certs",
	Short: "Manage HTTPS certificates",
	Long: `Manage HTTPS certificates for the reverse proxy.

Shows certificate status and allows regeneration of certificates.

Examples:
  prox certs              # Show certificate status
  prox certs --regenerate # Regenerate certificates`,
	RunE: runCerts,
}

func runCerts(cmd *cobra.Command, args []string) error {
	// Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if proxy is configured
	if cfg.Proxy == nil || !cfg.Proxy.Enabled {
		return fmt.Errorf("proxy is not configured or not enabled\nAdd a 'proxy' section to your prox.yaml to enable HTTPS proxy")
	}

	if cfg.Certs == nil {
		return fmt.Errorf("certs configuration missing")
	}

	// Create cert manager
	certMgr := certs.NewManager(cfg.Certs.Dir, cfg.Proxy.Domain)

	// Check mkcert installation
	if err := certMgr.CheckMkcert(); err != nil {
		fmt.Println("\nTo install mkcert:")
		fmt.Println("  macOS:   brew install mkcert")
		fmt.Println("  Linux:   See https://github.com/FiloSottile/mkcert#installation")
		fmt.Println("  Windows: choco install mkcert")
		return fmt.Errorf("mkcert not available: %w", err)
	}

	// Check CA installation
	caInstalled, err := certMgr.CheckCAInstalled()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not check CA status: %v\n", err)
	}

	// Print status
	fmt.Printf("Domain: %s\n", cfg.Proxy.Domain)
	fmt.Printf("Certs directory: %s\n", cfg.Certs.Dir)
	fmt.Println()

	if !caInstalled {
		fmt.Println("CA Status: Not installed")
		fmt.Println("Run 'mkcert -install' to install the CA (requires sudo)")
		return nil
	}
	fmt.Println("CA Status: Installed")

	paths := certMgr.GetCertPaths()
	fmt.Printf("Certificate: %s\n", paths.CertFile)
	fmt.Printf("Key: %s\n", paths.KeyFile)
	fmt.Println()

	if certsRegenerate {
		fmt.Println("Regenerating certificates...")
		_, err := certMgr.RegenerateCerts()
		if err != nil {
			return fmt.Errorf("failed to regenerate certificates: %w", err)
		}
		fmt.Println("Certificates regenerated successfully.")
		return nil
	}

	// Check if certs exist
	certPaths, err := certMgr.EnsureCerts()
	if err != nil {
		return fmt.Errorf("failed to ensure certificates: %w", err)
	}

	// Verify files exist
	if _, err := os.Stat(certPaths.CertFile); err == nil {
		fmt.Println("Status: Certificates exist and are ready")
	} else {
		fmt.Println("Status: Certificates will be generated on first use")
	}
	return nil
}

// Hosts command flags
var (
	hostsAdd    bool
	hostsRemove bool
	hostsShow   bool
)

// hostsCmd represents the hosts command
var hostsCmd = &cobra.Command{
	Use:   "hosts",
	Short: "Manage /etc/hosts entries",
	Long: `Manage /etc/hosts entries for the reverse proxy.

Adds or removes hostname entries that point to localhost for the
configured proxy services.

Examples:
  prox hosts          # Show current status and entries
  prox hosts --add    # Add proxy hosts to /etc/hosts
  prox hosts --remove # Remove proxy hosts from /etc/hosts
  prox hosts --show   # Show entries that would be added`,
	RunE: runHosts,
}

func init() {
	// Register commands
	rootCmd.AddCommand(certsCmd)
	rootCmd.AddCommand(hostsCmd)

	// Certs command flags
	certsCmd.Flags().BoolVar(&certsRegenerate, "regenerate", false, "Force regenerate certificates")

	// Hosts command flags
	hostsCmd.Flags().BoolVar(&hostsAdd, "add", false, "Add entries to /etc/hosts (requires sudo)")
	hostsCmd.Flags().BoolVar(&hostsRemove, "remove", false, "Remove entries from /etc/hosts (requires sudo)")
	hostsCmd.Flags().BoolVar(&hostsShow, "show", false, "Show entries that would be added")
}

func runHosts(cmd *cobra.Command, args []string) error {
	// Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Validate mutually exclusive flags
	flagsSet := 0
	if hostsAdd {
		flagsSet++
	}
	if hostsRemove {
		flagsSet++
	}
	if hostsShow {
		flagsSet++
	}
	if flagsSet > 1 {
		return fmt.Errorf("--add, --remove, and --show are mutually exclusive")
	}

	// Check if proxy is configured
	if cfg.Proxy == nil || !cfg.Proxy.Enabled {
		return fmt.Errorf("proxy is not configured or not enabled\nAdd a 'proxy' section to your prox.yaml to enable HTTPS proxy")
	}

	// Get service names
	serviceNames := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)

	if len(serviceNames) == 0 {
		fmt.Println("No services configured.")
		fmt.Println("Add a 'services' section to your prox.yaml to define service routing.")
		return nil
	}

	// Create hosts manager
	hostsMgr := hosts.NewManager(cfg.Proxy.Domain, serviceNames)

	if hostsShow || (!hostsAdd && !hostsRemove) {
		// Show current status and entries
		exists, upToDate, err := hostsMgr.Check()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not check hosts file: %v\n", err)
		}

		fmt.Printf("Domain: %s\n", cfg.Proxy.Domain)
		fmt.Println()

		// Show entries table
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tHOSTNAME")
		fmt.Fprintln(w, "-------\t--------")
		fmt.Fprintf(w, "(base)\t%s\n", cfg.Proxy.Domain)
		for _, name := range serviceNames {
			fmt.Fprintf(w, "%s\t%s.%s\n", name, name, cfg.Proxy.Domain)
		}
		w.Flush()
		fmt.Println()

		if exists {
			if upToDate {
				fmt.Println("Status: Entries are up to date in /etc/hosts")
			} else {
				fmt.Println("Status: Entries exist but need updating")
				fmt.Println("Run 'prox hosts --add' to update")
			}
		} else {
			fmt.Println("Status: Entries not in /etc/hosts")
			fmt.Println("Run 'prox hosts --add' to add entries (requires sudo)")
		}

		return nil
	}

	if hostsAdd {
		fmt.Println("Adding entries to /etc/hosts...")
		fmt.Println()
		fmt.Println("This will add the following block:")
		fmt.Println(hostsMgr.PrintEntries())

		if err := hostsMgr.Add(); err != nil {
			// Likely permission error - print sudo instructions
			fmt.Println()
			fmt.Println("You need elevated privileges to modify /etc/hosts.")
			fmt.Println("Run the following command manually:")
			fmt.Println()
			fmt.Println("  " + hostsMgr.GenerateAddCommand())
			return fmt.Errorf("failed to add hosts entries: %w", err)
		}
		fmt.Println("Entries added successfully.")
		return nil
	}

	if hostsRemove {
		fmt.Println("Removing entries from /etc/hosts...")
		if err := hostsMgr.Remove(); err != nil {
			fmt.Println()
			fmt.Println("You need elevated privileges to modify /etc/hosts.")
			fmt.Println("Run the following command manually:")
			fmt.Println()
			fmt.Println("  " + hostsMgr.GenerateRemoveCommand())
			return fmt.Errorf("failed to remove hosts entries: %w", err)
		}
		fmt.Println("Entries removed successfully.")
		return nil
	}
	return nil
}

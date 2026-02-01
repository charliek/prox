package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/proxy/certs"
	"github.com/charliek/prox/internal/proxy/hosts"
)

// cmdCerts handles the 'certs' command
func (a *App) cmdCerts(args []string) int {
	regenerate := false
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			fmt.Print(`prox certs - Manage HTTPS certificates

Usage: prox certs [options]

Options:
  --regenerate    Force regenerate certificates
  -h, --help      Show this help

Examples:
  prox certs              # Show certificate status
  prox certs --regenerate # Regenerate certificates
`)
			return 0
		case "--regenerate":
			regenerate = true
		}
	}

	// Load config
	cfg, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Check if proxy is configured
	if cfg.Proxy == nil || !cfg.Proxy.Enabled {
		fmt.Println("Proxy is not configured or not enabled.")
		fmt.Println("Add a 'proxy' section to your prox.yaml to enable HTTPS proxy.")
		return 0
	}

	if cfg.Certs == nil {
		fmt.Fprintf(os.Stderr, "Error: certs configuration missing\n")
		return 1
	}

	// Create cert manager
	certMgr := certs.NewManager(cfg.Certs.Dir, cfg.Proxy.Domain)

	// Check mkcert installation
	if err := certMgr.CheckMkcert(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Println("\nTo install mkcert:")
		fmt.Println("  macOS:   brew install mkcert")
		fmt.Println("  Linux:   See https://github.com/FiloSottile/mkcert#installation")
		fmt.Println("  Windows: choco install mkcert")
		return 1
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
		return 0
	}
	fmt.Println("CA Status: Installed")

	paths := certMgr.GetCertPaths()
	fmt.Printf("Certificate: %s\n", paths.CertFile)
	fmt.Printf("Key: %s\n", paths.KeyFile)
	fmt.Println()

	if regenerate {
		fmt.Println("Regenerating certificates...")
		_, err := certMgr.RegenerateCerts()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		fmt.Println("Certificates regenerated successfully.")
		return 0
	}

	// Check if certs exist
	certPaths, err := certMgr.EnsureCerts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error ensuring certificates: %v\n", err)
		return 1
	}

	// Verify files exist
	if _, err := os.Stat(certPaths.CertFile); err == nil {
		fmt.Println("Status: Certificates exist and are ready")
	} else {
		fmt.Println("Status: Certificates will be generated on first use")
	}

	return 0
}

// cmdHosts handles the 'hosts' command
func (a *App) cmdHosts(args []string) int {
	add := false
	remove := false
	show := false

	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			fmt.Print(`prox hosts - Manage /etc/hosts entries

Usage: prox hosts [options]

Options:
  --add       Add entries to /etc/hosts (requires sudo)
  --remove    Remove entries from /etc/hosts (requires sudo)
  --show      Show entries that would be added
  -h, --help  Show this help

Examples:
  prox hosts          # Show current status and entries
  prox hosts --add    # Add proxy hosts to /etc/hosts
  prox hosts --remove # Remove proxy hosts from /etc/hosts
`)
			return 0
		case "--add":
			add = true
		case "--remove":
			remove = true
		case "--show":
			show = true
		}
	}

	// Load config
	cfg, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Check if proxy is configured
	if cfg.Proxy == nil || !cfg.Proxy.Enabled {
		fmt.Println("Proxy is not configured or not enabled.")
		fmt.Println("Add a 'proxy' section to your prox.yaml to enable HTTPS proxy.")
		return 0
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
		return 0
	}

	// Create hosts manager
	hostsMgr := hosts.NewManager(cfg.Proxy.Domain, serviceNames)

	if show || (!add && !remove) {
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

		return 0
	}

	if add {
		fmt.Println("Adding entries to /etc/hosts...")
		fmt.Println()
		fmt.Println("This will add the following block:")
		fmt.Println(hostsMgr.PrintEntries())

		if err := hostsMgr.Add(); err != nil {
			// Likely permission error - print sudo instructions
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			fmt.Println()
			fmt.Println("You need elevated privileges to modify /etc/hosts.")
			fmt.Println("Run the following command manually:")
			fmt.Println()
			fmt.Println("  " + hostsMgr.GenerateAddCommand())
			return 1
		}
		fmt.Println("Entries added successfully.")
		return 0
	}

	if remove {
		fmt.Println("Removing entries from /etc/hosts...")
		if err := hostsMgr.Remove(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			fmt.Println()
			fmt.Println("You need elevated privileges to modify /etc/hosts.")
			fmt.Println("Run the following command manually:")
			fmt.Println()
			fmt.Println("  " + hostsMgr.GenerateRemoveCommand())
			return 1
		}
		fmt.Println("Entries removed successfully.")
		return 0
	}

	return 0
}

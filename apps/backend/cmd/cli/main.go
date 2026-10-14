package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/vtx/platform/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	switch command {
	case "doctor":
		runDoctor()
	case "worker":
		if len(os.Args) > 2 && os.Args[2] == "status" {
			runWorkerStatus()
		} else {
			fmt.Println("Usage: platform worker status")
			os.Exit(1)
		}
	case "version":
		fmt.Printf("VTX Platform CLI v1.0.0 (%s/%s)\n", runtime.GOOS, runtime.GOARCH)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Printf("Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("VTX Platform CLI")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  platform doctor          Run developer environment and dependency checks")
	fmt.Println("  platform worker status   Show Temporal worker status and configuration")
	fmt.Println("  platform version         Show CLI version information")
	fmt.Println("  platform help            Show this help message")
}

func runDoctor() {
	fmt.Println("🩺 Running VTX Platform Doctor...")
	fmt.Println("==================================================")

	allPassed := true

	// 1. Check Go
	fmt.Printf("%-20s", "Go toolchain:")
	goVer := runtime.Version()
	fmt.Printf(" [OK] %s\n", goVer)

	// 2. Check Node
	fmt.Printf("%-20s", "Node.js:")
	if nodeOut, err := exec.Command("node", "-v").Output(); err == nil {
		fmt.Printf(" [OK] %s", string(nodeOut))
	} else {
		fmt.Println(" [FAIL] Not found in PATH")
		allPassed = false
	}

	// 3. Check pnpm
	fmt.Printf("%-20s", "pnpm:")
	if pnpmOut, err := exec.Command("pnpm", "-v").Output(); err == nil {
		fmt.Printf(" [OK] v%s", string(pnpmOut))
	} else {
		fmt.Println(" [FAIL] Not found in PATH")
		allPassed = false
	}

	// 4. Check Docker
	fmt.Printf("%-20s", "Docker:")
	if dockerOut, err := exec.Command("docker", "--version").Output(); err == nil {
		fmt.Printf(" [OK] %s", strings.TrimSpace(string(dockerOut)))
	} else {
		fmt.Println(" [WARN] Docker not running or not in PATH")
	}
	fmt.Println()

	// 5. Check sqlc
	fmt.Printf("%-20s", "sqlc:")
	if sqlcOut, err := exec.Command("sqlc", "version").Output(); err == nil {
		fmt.Printf(" [OK] %s", strings.TrimSpace(string(sqlcOut)))
	} else {
		fmt.Println(" [WARN] sqlc not found (required for code generation)")
	}
	fmt.Println()

	// 6. Check Goose
	fmt.Printf("%-20s", "goose:")
	if gooseOut, err := exec.Command("goose", "-version").Output(); err == nil {
		fmt.Printf(" [OK] %s", strings.TrimSpace(string(gooseOut)))
	} else {
		fmt.Println(" [WARN] goose not found (required for migrations)")
	}
	fmt.Println()

	// 7. Check Database Connectivity
	cfg := config.Load()
	fmt.Printf("%-20s", "PostgreSQL (5432):")
	if checkTCP("localhost:5432", 1*time.Second) {
		fmt.Println(" [OK] Connected to localhost:5432")
	} else {
		fmt.Println(" [INFO] Not listening on localhost:5432 (start with 'make compose-up')")
	}

	// 8. Check Temporal Connectivity
	fmt.Printf("%-20s", "Temporal (7233):")
	if checkTCP(cfg.TemporalHostPort, 1*time.Second) {
		fmt.Printf(" [OK] Connected to %s\n", cfg.TemporalHostPort)
	} else {
		fmt.Printf(" [INFO] Not listening on %s (start with 'make compose-up')\n", cfg.TemporalHostPort)
	}

	fmt.Println("==================================================")
	if allPassed {
		fmt.Println("✨ Core toolchain prerequisites are satisfied.")
	} else {
		fmt.Println("⚠️  Some prerequisites are missing. Please install required tools.")
		os.Exit(1)
	}
}

func runWorkerStatus() {
	cfg := config.Load()
	fmt.Println("👷 VTX Worker Status (Read-Only)")
	fmt.Println("==================================================")
	fmt.Printf("Temporal Host:   %s\n", cfg.TemporalHostPort)
	fmt.Printf("Namespace:       %s\n", cfg.TemporalNamespace)
	fmt.Printf("Task Queue:      %s\n", cfg.TemporalQueue)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", cfg.TemporalHostPort)
	if err != nil {
		fmt.Printf("Temporal State:  OFFLINE (cannot connect to %s)\n", cfg.TemporalHostPort)
	} else {
		_ = conn.Close()
		fmt.Println("Temporal State:  ONLINE")
	}
	fmt.Println("==================================================")
}

func checkTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

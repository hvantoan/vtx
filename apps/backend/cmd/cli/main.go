package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/orchestration"
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
	case "workflow":
		if len(os.Args) > 2 && os.Args[2] == "run" {
			taskID := fmt.Sprintf("task-%d", time.Now().Unix())
			failStep := 0
			stepDelayMs := 0
			if len(os.Args) > 3 {
				taskID = os.Args[3]
			}
			if len(os.Args) > 4 {
				_, _ = fmt.Sscanf(os.Args[4], "%d", &failStep)
			}
			if len(os.Args) > 5 {
				_, _ = fmt.Sscanf(os.Args[5], "%d", &stepDelayMs)
			}
			runWorkflow(taskID, failStep, stepDelayMs)
		} else {
			fmt.Println("Usage: platform workflow run [task_id] [fail_step] [step_delay_ms]")
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
	fmt.Println("  platform workflow run    Trigger and observe a sample Temporal workflow")
	fmt.Println("  platform version         Show CLI version information")
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
	dbAddr := "localhost:49432"
	if u, err := url.Parse(cfg.DatabaseURL); err == nil && u.Host != "" {
		dbAddr = u.Host
	}
	fmt.Printf("%-20s", "PostgreSQL:")
	if checkTCP(dbAddr, 1*time.Second) {
		fmt.Printf(" [OK] Connected to %s\n", dbAddr)
	} else {
		fmt.Printf(" [INFO] Not listening on %s (start with 'make compose-up')\n", dbAddr)
	}

	// 8. Check Temporal Connectivity
	fmt.Printf("%-20s", "Temporal:")
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

func runWorkflow(taskID string, failStep int, stepDelayMs int) {
	cfg := config.Load()
	fmt.Printf("🚀 Triggering Temporal Workflow for Task: %s\n", taskID)
	fmt.Printf("Connecting to Temporal at %s (namespace: %s, queue: %s)...\n", cfg.TemporalHostPort, cfg.TemporalNamespace, cfg.TemporalQueue)

	c, err := client.Dial(client.Options{
		HostPort:  cfg.TemporalHostPort,
		Namespace: cfg.TemporalNamespace,
	})
	if err != nil {
		fmt.Printf("❌ Failed to connect to Temporal: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	workflowOptions := client.StartWorkflowOptions{
		ID:        "sample-workflow-" + taskID,
		TaskQueue: cfg.TemporalQueue,
	}

	input := orchestration.SampleWorkflowInput{
		TaskID:      taskID,
		Steps:       []string{"1-prepare-sandbox", "2-execute-code-change", "3-run-verification", "4-publish-results"},
		FailStep:    failStep,
		StepDelayMs: stepDelayMs,
	}

	ctx := context.Background()
	we, err := c.ExecuteWorkflow(ctx, workflowOptions, orchestration.SampleWorkflow, input)
	if err != nil {
		fmt.Printf("❌ Failed to start workflow: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Workflow Started successfully!\n")
	fmt.Printf("   WorkflowID: %s\n", we.GetID())
	fmt.Printf("   RunID:      %s\n", we.GetRunID())
	fmt.Println("⏳ Waiting for workflow completion...")

	var output orchestration.SampleWorkflowOutput
	err = we.Get(ctx, &output)
	if err != nil {
		fmt.Printf("❌ Workflow execution failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("🎉 Workflow Completed!\n")
	fmt.Printf("   Completed Steps: %v\n", output.CompletedSteps)
	fmt.Printf("   Duration:        %s\n", output.TotalDuration)
}

func checkTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

//go:build ignore

package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

var downloads = []struct {
	url  string
	dest string
}{
	{
		"https://unpkg.com/htmx.org@2.0.7/dist/htmx.min.js",
		"static/js/htmx.min.js",
	},
	{
		"https://github.com/tailwindlabs/tailwindcss/releases/download/v3.4.17/tailwindcss-linux-x64",
		"static/bin/tailwindcss",
	},
}

func main() {
	_ = os.Chdir("./internal/api")
	for _, dl := range downloads {
		fmt.Printf("Downloading %s\n", dl.url)
		if err := download(dl.url, dl.dest); err != nil {
			fmt.Fprintf(os.Stderr, "Error downloading %s: %v\n", dl.url, err)
			os.Exit(1)
		}
		fmt.Printf("  -> %s\n", dl.dest)
	}

	// Build Tailwind CSS
	fmt.Println("\nBuilding Tailwind CSS...")
	if err := buildTailwind(); err != nil {
		fmt.Fprintf(os.Stderr, "Error building Tailwind CSS: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Done!")
}

func download(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		return err
	}

	// Make tailwindcss binary executable
	if filepath.Base(dest) == "tailwindcss" {
		return os.Chmod(dest, 0755)
	}

	return nil
}

func buildTailwind() error {
	// Prefer local binary
	localBinary := "static/bin/tailwindcss"
	if _, err := os.Stat(localBinary); err == nil {
		cmd := exec.Command(localBinary, "-i", "static/css/input.css", "-o", "static/css/style.css", "--minify")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Check if npx is available
	if _, err := exec.LookPath("npx"); err == nil {
		cmd := exec.Command("npx", "tailwindcss", "-i", "static/css/input.css", "-o", "static/css/style.css", "--minify")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Check if tailwindcss CLI is available
	if _, err := exec.LookPath("tailwindcss"); err == nil {
		cmd := exec.Command("tailwindcss", "-i", "static/css/input.css", "-o", "static/css/style.css", "--minify")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	return fmt.Errorf("tailwindcss CLI not found. Run `go generate` to download it")
}

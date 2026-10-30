// Command mymedical runs the app: a household's medical records on their
// own server, with a local model to read them and answer questions.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/DeGrandis/mymedical/internal/app"
	"github.com/DeGrandis/mymedical/internal/brand"
	"github.com/DeGrandis/mymedical/internal/reference"
	"github.com/DeGrandis/mymedical/web"
)

var version = "dev"

func env(name, def string) string {
	if v := os.Getenv(strings.ToUpper(brand.Code) + "_" + name); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "reference" && os.Args[2] == "bundle-medlineplus" {
		bundleMedlinePlus(os.Args[3:])
		return
	}
	addr := flag.String("addr", env("ADDR", "127.0.0.1:8080"), "listen address")
	data := flag.String("data", env("DATA", "./data"), "data directory")
	flag.Parse()

	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	a, err := app.New(app.Config{
		DataDir:   *data,
		Version:   version,
		Password:  env("PASSWORD", ""),
		OllamaURL: env("OLLAMA_URL", ""),
		Web:       web.Static(),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	srv := &http.Server{Addr: *addr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		fmt.Printf("%s %s on http://%s (data in %s)\n", brand.Name, version, *addr, *data)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// bundleMedlinePlus refreshes the MedlinePlus topics built into the
// binary: go run ./cmd/mymedical reference bundle-medlineplus
func bundleMedlinePlus(args []string) {
	fs := flag.NewFlagSet("bundle-medlineplus", flag.ExitOnError)
	out := fs.String("out", "internal/reference/data/medlineplus.json.gz", "where to write the bundle")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	version, topics, err := reference.FetchTopics(ctx, &http.Client{})
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	if err := reference.BundleTopics(f, version, topics); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d MedlinePlus topics from %s written to %s\n", len(topics), version, *out)
}

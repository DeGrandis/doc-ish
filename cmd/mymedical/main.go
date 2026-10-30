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

package main

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/nomanoma121/snappy/api/v1alpha1"
	"github.com/nomanoma121/snappy/internal/forge"
	"github.com/nomanoma121/snappy/internal/server"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("failed to load .env file")
	}

	addr := mustEnv("SERVER_ADDR")
	appIDStr := mustEnv("GITHUB_APP_ID")
	privateKeyPath := mustEnv("GITHUB_PRIVATE_KEY_PATH")

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		log.Fatalf("invalid GITHUB_APP_ID: %v", err)
	}

	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		log.Fatalf("failed to read private key: %v", err)
	}

	ghClient := forge.NewGitHubClient(appID, privateKey)

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("failed to get kubeconfig: %v", err)
	}
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("failed to create k8s client: %v", err)
	}

	srv := server.NewServer(addr, ghClient, k8sClient)
	log.Printf("starting server on %s", addr)
	if err := srv.Start(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	authorizationv1 "github.com/agynio/image-catalog/gen/agynio/api/authorization/v1"
	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	notificationsv1 "github.com/agynio/image-catalog/gen/agynio/api/notifications/v1"
	secretsv1 "github.com/agynio/image-catalog/gen/agynio/api/secrets/v1"
	"github.com/agynio/image-catalog/internal/config"
	"github.com/agynio/image-catalog/internal/db"
	"github.com/agynio/image-catalog/internal/discovery"
	"github.com/agynio/image-catalog/internal/registry"
	"github.com/agynio/image-catalog/internal/server"
	"github.com/agynio/image-catalog/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("images: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}
	defer pool.Close()

	if err := db.ApplyMigrations(ctx, pool); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	authzConn, err := dial(cfg.AuthorizationGRPCTarget)
	if err != nil {
		return fmt.Errorf("dial authorization: %w", err)
	}
	defer authzConn.Close()

	secretsConn, err := dial(cfg.SecretsGRPCTarget)
	if err != nil {
		return fmt.Errorf("dial secrets: %w", err)
	}
	defer secretsConn.Close()

	imagesStore := store.NewStore(pool)
	imagesServer := server.New(
		imagesStore,
		authorizationv1.NewAuthorizationServiceClient(authzConn),
		secretsv1.NewSecretsServiceClient(secretsConn),
		registry.New(),
	)

	if cfg.NotificationsGRPCTarget != "" {
		notificationsConn, err := dial(cfg.NotificationsGRPCTarget)
		if err != nil {
			return fmt.Errorf("dial notifications: %w", err)
		}
		defer notificationsConn.Close()
		imagesServer.WithNotifications(notificationsv1.NewNotificationsServiceClient(notificationsConn))
	}

	// The server resolves credentials and publishes updates, so it is both the
	// discoverer's credential resolver and its publisher.
	discoverer := discovery.New(imagesStore, registry.New(), imagesServer, imagesServer, discovery.Options{
		Interval:  cfg.DiscoveryInterval,
		Timeout:   cfg.DiscoveryTimeout,
		BatchSize: cfg.DiscoveryBatchSize,
	})
	imagesServer.WithDiscoverer(discoverer)
	go discoverer.Run(ctx)

	grpcServer := grpc.NewServer()
	imagesv1.RegisterImagesServiceServer(grpcServer, imagesServer)

	lis, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddress, err)
	}

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	log.Printf("ImagesService listening on %s", cfg.GRPCAddress)

	if err := grpcServer.Serve(lis); err != nil {
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func dial(target string) (*grpc.ClientConn, error) {
	return grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

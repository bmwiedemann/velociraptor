//
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"gopkg.in/alecthomas/kingpin.v2"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"time"
	"www.velocidex.com/golang/velociraptor/config"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	"www.velocidex.com/golang/velociraptor/logging"
	"www.velocidex.com/golang/velociraptor/server"
)

var (
	healthy int32
)

func validateServerConfig(configuration *config.Config) error {
	if configuration.Frontend.Certificate == "" {
		return errors.New("Configuration does not specify a frontend certificate.")
	}

	return nil
}

func get_server_config(config_path string) (*config.Config, error) {
	config_obj := config.GetDefaultConfig()
	err := config.LoadConfig(config_path, config_obj)
	if err == nil {
		err = validateServerConfig(config_obj)
	}

	return config_obj, err
}

func get_config(config_path string) (*config.Config, error) {
	config_obj := config.GetDefaultConfig()
	err := config.LoadConfig(config_path, config_obj)
	return config_obj, err
}

func start_frontend(config_obj *config.Config) {
	server_obj, err := server.NewServer(config_obj)
	kingpin.FatalIfError(err, "Unable to create server")

	router := http.NewServeMux()
	router.Handle("/healthz", healthz())
	router.Handle("/server.pem", server_pem(config_obj))

	router.Handle("/control", control(server_obj))

	listenAddr := fmt.Sprintf(
		"%s:%d",
		config_obj.Frontend.BindAddress,
		config_obj.Frontend.BindPort)

	server := &http.Server{
		Addr:        listenAddr,
		Handler:     logging.GetLoggingHandler(config_obj)(router),
		ReadTimeout: 5 * time.Second,
		// Our write timeout is controlled by the handler.
		WriteTimeout: 900 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	done := make(chan bool)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)

	go func() {
		<-quit
		server_obj.Info("Server is shutting down...")
		atomic.StoreInt32(&healthy, 0)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			kingpin.Fatalf(
				"Could not gracefully shutdown the server: %v\n", err)
		}
		close(done)
	}()

	server_obj.Info("Frontend is ready to handle client requests at %s", listenAddr)
	atomic.StoreInt32(&healthy, 1)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		kingpin.Fatalf("Could not listen on %s: %v\n", listenAddr, err)
	}

	<-done
	server_obj.Info("Server stopped")
}

func healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

func server_pem(config_obj *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()

		w.Write([]byte(config_obj.Frontend.Certificate))
	})
}

func control(server_obj *server.Server) http.Handler {
	pad := &crypto_proto.ClientCommunication{}
	pad.Padding = append(pad.Padding, 0)
	serialized_pad, _ := proto.Marshal(pad)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			panic("http handler is not a flusher")
		}

		body, err := ioutil.ReadAll(req.Body)
		if err != nil {
			server_obj.Error("Unable to read body", err)
			http.Error(w, "", http.StatusServiceUnavailable)
			return
		}

		message_info, err := server_obj.Decrypt(req.Context(), body)
		if err != nil {
			server_obj.Error("Unable to decrypt body", err)
			http.Error(w, "", http.StatusServiceUnavailable)
			return
		}
		message_info.RemoteAddr = req.RemoteAddr

		// Very few Unauthenticated client messages are valid
		// - currently only enrolment requests.
		if !message_info.Authenticated {
			err := server_obj.ProcessUnauthenticatedMessages(
				req.Context(), message_info)
			if err == nil {
				// If we can not decrypt the message
				// because we do not know about this
				// client, we need to indicate to the
				// client to start the enrolment
				// process. Since the client can not
				// read anything from us (because we
				// can not encrypt for it), we
				// indicate this by providing it with
				// an HTTP error code.
				http.Error(
					w,
					"Please Enrol",
					http.StatusNotAcceptable)
			} else {
				server_obj.Error("Unable to process", err)
				http.Error(w, "", http.StatusServiceUnavailable)
			}
			return
		}

		// From here below we have received the client payload
		// and it should not resend it to us. We need to keep
		// the client blocked until we finish processing the
		// flow as a method of rate limiting the clients. We
		// do this by streaming pad packets to the client,
		// while the flow is processed.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		sync := make(chan []byte)
		go func() {
			defer close(sync)
			response, err := server_obj.Process(req.Context(), message_info)
			if err != nil {
				server_obj.Error("Error:", err)
			} else {
				sync <- response
			}
		}()

		// Spin here on the response being ready, every few
		// seconds we send a pad packet to the client to keep
		// the connection active. The pad packet is sent using
		// chunked encoding and will be assembled by the
		// client into the complete protobuf when it is
		// read. Since protobuf serialization does not have
		// headers, it is safe to just concat multiple binary
		// encodings into a single proto. This means the
		// decoded protobuf will actually contain the full
		// response as well as the padding fields (which are
		// ignored).
		for {
			select {
			case response := <-sync:
				w.Write(response)
				return

			case <-time.After(3 * time.Second):
				w.Write(serialized_pad)
				flusher.Flush()
			}
		}
	})
}
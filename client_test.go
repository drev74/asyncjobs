// Copyright (c) 2022, R.I. Pienaar and the Project contributors
//
// SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/jsm.go"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAsyncJobs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "AsyncJobs")
}

func withJetStream(cb func(nc *nats.Conn, mgr *jsm.Manager)) {
	d, err := ioutil.TempDir("", "jstest")
	Expect(err).ToNot(HaveOccurred())
	defer os.RemoveAll(d)

	opts := &server.Options{
		JetStream: true,
		StoreDir:  d,
		Port:      -1,
		Host:      "localhost",
		LogFile:   "/dev/stdout",
		Trace:     false,
	}

	s, err := server.NewServer(opts)
	Expect(err).ToNot(HaveOccurred())

	// s.ConfigureLogger()

	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		Fail("nats server did not start")
	}
	defer func() {
		s.Shutdown()
		s.WaitForShutdown()
	}()

	nc, err := nats.Connect(s.ClientURL(), nats.UseOldRequestStyle())
	Expect(err).ToNot(HaveOccurred())
	defer nc.Close()

	mgr, err := jsm.New(nc, jsm.WithTimeout(time.Second))
	Expect(err).ToNot(HaveOccurred())

	cb(nc, mgr)
}

var _ = Describe("Client", func() {
	BeforeEach(func() {
		log.SetOutput(GinkgoWriter)
	})

	Describe("shouldDiscardTask", func() {
		It("Should correctly detect task states", func() {
			withJetStream(func(nc *nats.Conn, mgr *jsm.Manager) {
				_, err := NewClient(NatsConn(nc), DiscardTaskStates(TaskStateExpired, TaskStateActive))
				Expect(err).To(MatchError("only states completed, expired or terminated can be discarded"))

				client, err := NewClient(NatsConn(nc), DiscardTaskStates(TaskStateExpired, TaskStateCompleted))
				Expect(err).ToNot(HaveOccurred())

				Expect(client.shouldDiscardTask(&Task{State: TaskStateActive})).To(BeFalse())
				Expect(client.shouldDiscardTask(&Task{State: TaskStateExpired})).To(BeTrue())
				Expect(client.shouldDiscardTask(&Task{State: TaskStateCompleted})).To(BeTrue())
			})
		})
	})

	Describe("discardTaskIfDesired", func() {
		It("Should delete the correct tasks", func() {
			withJetStream(func(nc *nats.Conn, mgr *jsm.Manager) {
				client, err := NewClient(NatsConn(nc), DiscardTaskStates(TaskStateExpired, TaskStateCompleted))
				Expect(err).ToNot(HaveOccurred())

				task, err := NewTask("x", nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(client.EnqueueTask(context.Background(), task)).ToNot(HaveOccurred())

				Expect(client.discardTaskIfDesired(task)).ToNot(HaveOccurred())
				_, err = client.LoadTaskByID(task.ID)
				Expect(err).ToNot(HaveOccurred())

				task.State = TaskStateExpired
				Expect(client.discardTaskIfDesired(task)).ToNot(HaveOccurred())
				_, err = client.LoadTaskByID(task.ID)
				Expect(err).To(MatchError("task not found"))
			})
		})
	})

	It("Should function", func() {
		Skip("For interactive testing and debugging")
		withJetStream(func(nc *nats.Conn, mgr *jsm.Manager) {
			client, err := NewClient(NatsConn(nc), RetryBackoffPolicy(RetryLinearOneMinute))
			Expect(err).ToNot(HaveOccurred())

			testCount := 1000
			firstID := ""

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			for i := 0; i < testCount; i++ {
				task, err := NewTask("test", map[string]string{"hello": "world"})
				Expect(err).ToNot(HaveOccurred())

				if firstID == "" {
					firstID = task.ID
				}

				err = client.EnqueueTask(ctx, task)
				Expect(err).ToNot(HaveOccurred())
			}

			log.Printf("Starting processing")

			handled := int32(0)

			router := NewTaskRouter()
			router.HandleFunc("test", func(ctx context.Context, t *Task) (interface{}, error) {
				if t.Tries > 1 {
					log.Printf("Try %d for task %s", t.Tries, t.ID)
				}

				done := atomic.AddInt32(&handled, 1)
				if done == int32(testCount)+10 {
					log.Printf("Processed all messages")
					time.AfterFunc(50*time.Millisecond, func() {
						cancel()
					})
				} else if done > 0 && done%100 == 0 {
					return nil, fmt.Errorf("simulated error on %d", done)
				}

				return []byte("done"), nil
			})

			err = client.Run(ctx, router)
			if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
				Fail(fmt.Sprintf("client run failed: %s", err))
			}

			task, err := client.LoadTaskByID(firstID)
			if err != nil {
				Fail(fmt.Sprintf("load failed: %v", err))
			}
			if task.State == TaskStateCompleted {
				tj, _ := json.MarshalIndent(task, "", "  ")
				fmt.Printf("Task: %s\n", string(tj))
				fmt.Printf("%s task %s @ %s result: %q", task.State, task.ID, task.Result.CompletedAt, task.Result.Payload)
			} else {
				fmt.Printf("error task: %#v", task)
			}
		})
	})

	It("Should handle retried messages with a backoff delay", func() {
		withJetStream(func(nc *nats.Conn, _ *jsm.Manager) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := NewClient(NatsConn(nc), RetryBackoffPolicy(retryForTesting))
			Expect(err).ToNot(HaveOccurred())

			task, err := NewTask("ginkgo", "test")
			Expect(err).ToNot(HaveOccurred())
			Expect(client.EnqueueTask(ctx, task)).ToNot(HaveOccurred())
			task, err = NewTask("ginkgo", "test")
			Expect(err).ToNot(HaveOccurred())
			Expect(client.EnqueueTask(ctx, task)).ToNot(HaveOccurred())
			task, err = NewTask("ginkgo", "test")
			Expect(err).ToNot(HaveOccurred())
			Expect(client.EnqueueTask(ctx, task)).ToNot(HaveOccurred())

			wg := sync.WaitGroup{}
			wg.Add(3)
			var tries []time.Time

			router := NewTaskRouter()
			router.HandleFunc("ginkgo", func(ctx context.Context, t *Task) (interface{}, error) {
				tries = append(tries, time.Now())

				log.Printf("Trying task %s on try %d\n", t.ID, t.Tries)

				if t.Tries < 2 {
					return "fail", fmt.Errorf("simulated failure")
				}

				wg.Done()
				return "done", nil
			})

			go client.Run(ctx, router)

			wg.Wait()

			Expect(len(tries)).To(Equal(6))
			task, err = client.LoadTaskByID(task.ID)
			Expect(err).ToNot(HaveOccurred())
			Expect(task.State).To(Equal(TaskStateCompleted))
			Expect(task.Tries).To(Equal(2))
		})
	})
})

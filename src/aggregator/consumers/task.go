package consumers

import (
	"strconv"

	"github.com/Shopify/sarama"
	log "github.com/Sirupsen/logrus"
	saramaC "github.com/d33d33/sarama-cluster"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/viper"

	"github.com/runabove/metronome/src/metronome/kafka"
	"github.com/runabove/metronome/src/metronome/models"
	"github.com/runabove/metronome/src/metronome/pg"
	"github.com/runabove/metronome/src/metronome/redis"
)

// TaskConsumer consumed tasks messages from a Kafka topic to maintain the tasks database.
type TaskConsumer struct {
	consumer                 *saramaC.Consumer
	taskCounter              *prometheus.CounterVec
	taskUnprocessableCounter *prometheus.CounterVec
	taskProcessedCounter     *prometheus.CounterVec
	taskPublishErrorCounter  *prometheus.CounterVec
}

// NewTaskConsumer returns a new task consumer.
func NewTaskConsumer() (*TaskConsumer, error) {
	brokers := viper.GetStringSlice("kafka.brokers")

	config := saramaC.NewConfig()
	config.Config = *kafka.NewConfig()
	config.ClientID = "metronome-aggregator"
	config.Consumer.Offsets.Initial = sarama.OffsetOldest

	consumer, err := saramaC.NewConsumer(brokers, kafka.GroupAggregators(), []string{kafka.TopicTasks()}, config)
	if err != nil {
		return nil, err
	}

	tc := &TaskConsumer{
		consumer: consumer,
	}

	// metrics
	tc.taskCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "metronome",
		Subsystem: "aggregator",
		Name:      "tasks",
		Help:      "Number of tasks processed.",
	},
		[]string{"partition"})
	prometheus.MustRegister(tc.taskCounter)
	tc.taskUnprocessableCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "metronome",
		Subsystem: "aggregator",
		Name:      "tasks_unprocessable",
		Help:      "Number of unprocessable tasks.",
	},
		[]string{"partition"})
	prometheus.MustRegister(tc.taskUnprocessableCounter)
	tc.taskProcessedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "metronome",
		Subsystem: "aggregator",
		Name:      "tasks_processeed",
		Help:      "Number of processeed tasks.",
	},
		[]string{"partition"})
	prometheus.MustRegister(tc.taskProcessedCounter)
	tc.taskPublishErrorCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "metronome",
		Subsystem: "aggregator",
		Name:      "tasks_publish_error",
		Help:      "Number of tasks publish error.",
	},
		[]string{"partition"})
	prometheus.MustRegister(tc.taskPublishErrorCounter)

	go func() {
		for {
			select {
			case msg, ok := <-consumer.Messages():
				if !ok { // shuting down
					return
				}
				tc.handleMsg(msg)
			}
		}
	}()

	return tc, nil
}

// Close the consumer.
func (tc *TaskConsumer) Close() error {
	return tc.consumer.Close()
}

// Handle message from Kafka.
// Apply updates to the database.
func (tc *TaskConsumer) handleMsg(msg *sarama.ConsumerMessage) {
	tc.taskCounter.WithLabelValues(strconv.Itoa(int(msg.Partition))).Inc()
	var t models.Task
	if err := t.FromKafka(msg); err != nil {
		tc.taskUnprocessableCounter.WithLabelValues(strconv.Itoa(int(msg.Partition))).Inc()
		log.Error(err)
		return
	}

	db := pg.DB()

	if t.Schedule == "" {
		log.Infof("DELETE task: %s", t.GUID)

		_, err := db.Model(&t).Delete()
		if err != nil {
			log.Errorf("%v", err)
		}
	} else {
		log.Infof("UPSERT task: %s", t.GUID)

		_, err := db.Model(&t).OnConflict("(guid) DO UPDATE").Set("name = ?name").Set("urn = ?urn").Set("schedule = ?schedule").Insert()
		if err != nil {
			log.Errorf("%v", err)
		}
	}
	tc.taskProcessedCounter.WithLabelValues(strconv.Itoa(int(msg.Partition))).Inc()

	if err := redis.DB().PublishTopic(t.UserID, "task", t.ToJSON()).Err(); err != nil {
		tc.taskPublishErrorCounter.WithLabelValues(strconv.Itoa(int(msg.Partition))).Inc()
		log.Error(err)
	}
}

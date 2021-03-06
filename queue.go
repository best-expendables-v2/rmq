package rmq

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/adjust/uniuri"
	"github.com/go-redis/redis/v8"
)

const (
	connectionsKey                   = "rmq::connections"                                           // Set of connection names
	connectionHeartbeatTemplate      = "rmq::connection::{connection}::heartbeat"                   // expires after {connection} died
	connectionQueuesTemplate         = "rmq::connection::{connection}::queues"                      // Set of queues consumers of {connection} are consuming
	connectionQueueConsumersTemplate = "rmq::connection::{connection}::queue::[{queue}]::consumers" // Set of all consumers from {connection} consuming from {queue}
	connectionQueueUnackedTemplate   = "rmq::connection::{connection}::queue::[{queue}]::unacked"   // List of deliveries consumers of {connection} are currently consuming

	queuesKey             = "rmq::queues"                     // Set of all open queues
	queueReadyTemplate    = "rmq::queue::[{queue}]::ready"    // List of deliveries in that {queue} (right is first and oldest, left is last and youngest)
	queueRejectedTemplate = "rmq::queue::[{queue}]::rejected" // List of rejected deliveries from that {queue}
	queueDelayedTemplate  = "rmq::queue::[{queue}]::delayed"  // List of delayed deliveries from that {queue}

	phConnection = "{connection}" // connection name
	phQueue      = "{queue}"      // queue name
	phConsumer   = "{consumer}"   // consumer name (consisting of tag and token)

	defaultBatchTimeout = time.Second
	purgeBatchSize      = 100
)

type Queue interface {
	Publish(payload string) bool
	PublishOnDelay(payload string, delayedAt time.Time) bool
	PublishBytes(payload []byte) bool
	PublishBytesOnDelay(payload []byte, delayedAt time.Time) bool
	PublishRejected(payload string) bool
	SetPushQueue(pushQueue Queue)
	StartConsuming(prefetchLimit int, pollDuration time.Duration) bool
	StopConsuming() bool
	AddConsumer(tag string, consumer Consumer) string
	AddBatchConsumer(tag string, batchSize int, consumer BatchConsumer) string
	AddBatchConsumerWithTimeout(tag string, batchSize int, timeout time.Duration, consumer BatchConsumer) string
	PurgeReady() int
	PurgeRejected() int
	ReturnRejected(count int) int
	ReturnAllRejected() int
	Close() bool
	ReadyCount() int
	RejectedCount() int
	UnackedCount() int
}

type redisQueue struct {
	name             string
	connectionName   string
	queuesKey        string // key to list of queues consumed by this connection
	consumersKey     string // key to set of consumers using this connection
	readyKey         string // key to list of ready deliveries
	rejectedKey      string // key to list of rejected deliveries
	unackedKey       string // key to list of currently consuming deliveries
	pushKey          string // key to list of pushed deliveries
	delayedKey       string // key to list of currently consuming deliveries
	redisClient      *redis.Client
	deliveryChan     chan Delivery // nil for publish channels, not nil for consuming channels
	prefetchLimit    int           // max number of prefetched deliveries number of unacked can go up to prefetchLimit + numConsumers
	pollDuration     time.Duration
	consumingStopped bool
}

func newQueue(name, connectionName, queuesKey string, redisClient *redis.Client) *redisQueue {
	consumersKey := strings.Replace(connectionQueueConsumersTemplate, phConnection, connectionName, 1)
	consumersKey = strings.Replace(consumersKey, phQueue, name, 1)

	readyKey := strings.Replace(queueReadyTemplate, phQueue, name, 1)
	rejectedKey := strings.Replace(queueRejectedTemplate, phQueue, name, 1)
	delayedKey := strings.Replace(queueDelayedTemplate, phQueue, name, 1)

	unackedKey := strings.Replace(connectionQueueUnackedTemplate, phConnection, connectionName, 1)
	unackedKey = strings.Replace(unackedKey, phQueue, name, 1)

	queue := &redisQueue{
		name:           name,
		connectionName: connectionName,
		queuesKey:      queuesKey,
		consumersKey:   consumersKey,
		readyKey:       readyKey,
		rejectedKey:    rejectedKey,
		unackedKey:     unackedKey,
		delayedKey:     delayedKey,
		redisClient:    redisClient,
	}
	return queue
}

func (queue *redisQueue) String() string {
	return fmt.Sprintf("[%s conn:%s]", queue.name, queue.connectionName)
}

// Publish adds a delivery with the given payload to the queue
func (queue *redisQueue) Publish(payload string) bool {
	// debug(fmt.Sprintf("publish %s %s", payload, queue)) // COMMENTOUT
	return !redisErrIsNil(queue.redisClient.LPush(context.Background(), queue.readyKey, payload))
}

func (queue *redisQueue) PublishOnDelay(payload string, delayedAt time.Time) bool {
	z := redis.Z{
		Score:  float64(delayedAt.Unix()),
		Member: payload,
	}

	result := queue.redisClient.ZAdd(context.Background(), queue.delayedKey, &z)
	return !redisErrIsNil(result)
}

// PublishBytes just casts the bytes and calls Publish
func (queue *redisQueue) PublishBytes(payload []byte) bool {
	return queue.Publish(string(payload))
}

func (queue *redisQueue) PublishBytesOnDelay(payload []byte, delayedAt time.Time) bool {
	return queue.PublishOnDelay(string(payload), delayedAt)
}

// Publish rejected job to rejected queue
func (queue *redisQueue) PublishRejected(payload string) bool {
	if redisErrIsNil(queue.redisClient.LPush(context.Background(), queue.rejectedKey, payload)) {
		return false
	}

	if redisErrIsNil(queue.redisClient.LRem(context.Background(), queue.unackedKey, 1, payload)) {
		return false
	}

	return true
}

// PurgeReady removes all ready deliveries from the queue and returns the number of purged deliveries
func (queue *redisQueue) PurgeReady() int {
	return queue.deleteRedisList(queue.readyKey)
}

// PurgeRejected removes all rejected deliveries from the queue and returns the number of purged deliveries
func (queue *redisQueue) PurgeRejected() int {
	return queue.deleteRedisList(queue.rejectedKey)
}

func (queue *redisQueue) PurgeDelayed() int {
	return queue.deleteRedisSortedSet(queue.rejectedKey)
}

// Close purges and removes the queue from the list of queues
func (queue *redisQueue) Close() bool {
	queue.PurgeRejected()
	queue.PurgeReady()
	result := queue.redisClient.SRem(context.Background(), queuesKey, queue.name)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

func (queue *redisQueue) ReadyCount() int {
	result := queue.redisClient.LLen(context.Background(), queue.readyKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) UnackedCount() int {
	result := queue.redisClient.LLen(context.Background(), queue.unackedKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) RejectedCount() int {
	result := queue.redisClient.LLen(context.Background(), queue.rejectedKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) DelayedCount() int {
	result := queue.redisClient.ZCount(context.Background(), queue.delayedKey, "-inf", "+inf")
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

// ReturnAllUnacked moves all unacked deliveries back to the ready
// queue and deletes the unacked key afterwards, returns number of returned
// deliveries
func (queue *redisQueue) ReturnAllUnacked() int {
	result := queue.redisClient.LLen(context.Background(), queue.unackedKey)
	if redisErrIsNil(result) {
		return 0
	}

	unackedCount := int(result.Val())
	for i := 0; i < unackedCount; i++ {
		if redisErrIsNil(queue.redisClient.RPopLPush(context.Background(), queue.unackedKey, queue.readyKey)) {
			return i
		}
		// debug(fmt.Sprintf("rmq queue returned unacked delivery %s %s", result.Val(), queue.readyKey)) // COMMENTOUT
	}

	return unackedCount
}

// ReturnAllRejected moves all rejected deliveries back to the ready
// list and returns the number of returned deliveries
func (queue *redisQueue) ReturnAllRejected() int {
	result := queue.redisClient.LLen(context.Background(), queue.rejectedKey)
	if redisErrIsNil(result) {
		return 0
	}

	rejectedCount := int(result.Val())
	return queue.ReturnRejected(rejectedCount)
}

// ReturnRejected tries to return count rejected deliveries back to
// the ready list and returns the number of returned deliveries
func (queue *redisQueue) ReturnRejected(count int) int {
	if count == 0 {
		return 0
	}

	for i := 0; i < count; i++ {
		result := queue.redisClient.RPopLPush(context.Background(), queue.rejectedKey, queue.readyKey)
		if redisErrIsNil(result) {
			return i
		}
		// debug(fmt.Sprintf("rmq queue returned rejected delivery %s %s", result.Val(), queue.readyKey)) // COMMENTOUT
	}

	return count
}

// CloseInConnection closes the queue in the associated connection by removing all related keys
func (queue *redisQueue) CloseInConnection() {
	redisErrIsNil(queue.redisClient.Del(context.Background(), queue.unackedKey))
	redisErrIsNil(queue.redisClient.Del(context.Background(), queue.consumersKey))
	redisErrIsNil(queue.redisClient.SRem(context.Background(), queue.queuesKey, queue.name))
}

func (queue *redisQueue) SetPushQueue(pushQueue Queue) {
	redisPushQueue, ok := pushQueue.(*redisQueue)
	if !ok {
		return
	}

	queue.pushKey = redisPushQueue.readyKey
}

// StartConsuming starts consuming into a channel of size prefetchLimit
// must be called before consumers can be added!
// pollDuration is the duration the queue sleeps before checking for new deliveries
func (queue *redisQueue) StartConsuming(prefetchLimit int, pollDuration time.Duration) bool {
	if queue.deliveryChan != nil {
		return false // already consuming
	}

	// add queue to list of queues consumed on this connection
	if redisErrIsNil(queue.redisClient.SAdd(context.Background(), queue.queuesKey, queue.name)) {
		log.Panicf("rmq queue failed to start consuming %s", queue)
	}

	queue.prefetchLimit = prefetchLimit
	queue.pollDuration = pollDuration
	queue.deliveryChan = make(chan Delivery, prefetchLimit)
	// log.Printf("rmq queue started consuming %s %d %s", queue, prefetchLimit, pollDuration)
	go queue.consume()
	return true
}

func (queue *redisQueue) StopConsuming() bool {
	if queue.deliveryChan == nil || queue.consumingStopped {
		return false // not consuming or already stopped
	}

	queue.consumingStopped = true
	return true
}

// AddConsumer adds a consumer to the queue and returns its internal name
// panics if StartConsuming wasn't called before!
func (queue *redisQueue) AddConsumer(tag string, consumer Consumer) string {
	name := queue.addConsumer(tag)
	go queue.consumerConsume(consumer)
	return name
}

// AddBatchConsumer is similar to AddConsumer, but for batches of deliveries
func (queue *redisQueue) AddBatchConsumer(tag string, batchSize int, consumer BatchConsumer) string {
	return queue.AddBatchConsumerWithTimeout(tag, batchSize, defaultBatchTimeout, consumer)
}

func (queue *redisQueue) AddBatchConsumerWithTimeout(tag string, batchSize int, timeout time.Duration, consumer BatchConsumer) string {
	name := queue.addConsumer(tag)
	go queue.consumerBatchConsume(batchSize, timeout, consumer)
	return name
}

func (queue *redisQueue) GetConsumers() []string {
	result := queue.redisClient.SMembers(context.Background(), queue.consumersKey)
	if redisErrIsNil(result) {
		return []string{}
	}
	return result.Val()
}

func (queue *redisQueue) RemoveConsumer(name string) bool {
	result := queue.redisClient.SRem(context.Background(), queue.consumersKey, name)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

func (queue *redisQueue) addConsumer(tag string) string {
	if queue.deliveryChan == nil {
		log.Panicf("rmq queue failed to add consumer, call StartConsuming first! %s", queue)
	}

	name := fmt.Sprintf("%s-%s", tag, uniuri.NewLen(6))

	// add consumer to list of consumers of this queue
	if redisErrIsNil(queue.redisClient.SAdd(context.Background(), queue.consumersKey, name)) {
		log.Panicf("rmq queue failed to add consumer %s %s", queue, tag)
	}

	// log.Printf("rmq queue added consumer %s %s", queue, name)
	return name
}

func (queue *redisQueue) RemoveAllConsumers() int {
	result := queue.redisClient.Del(context.Background(), queue.consumersKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) consume() {
	for {
		queue.migrateExpiredDeliveries(queue.delayedKey, queue.readyKey, time.Now())

		batchSize := queue.batchSize()
		wantMore := queue.consumeBatch(batchSize)

		if !wantMore {
			time.Sleep(queue.pollDuration)
		}

		if queue.consumingStopped {
			// log.Printf("rmq queue stopped consuming %s", queue)
			return
		}
	}
}

func (queue *redisQueue) migrateExpiredDeliveries(from string, to string, curr time.Time) bool {
	cmd := queue.redisClient.Eval(context.Background(),
		`-- Get all of the jobs with an expired "score"...
		local val = redis.call('zrangebyscore', KEYS[1], '-inf', ARGV[1])

		-- If we have values in the array, we will remove them from the first queue
		-- and add them onto the destination queue in chunks of 100, which moves
		-- all of the appropriate jobs onto the destination queue very safely.
		if(next(val) ~= nil) then
			redis.call('zremrangebyrank', KEYS[1], 0, #val - 1)

			for i = 1, #val, 100 do
				redis.call('rpush', KEYS[2], unpack(val, i, math.min(i+99, #val)))
			end
		end

		return val`,
		[]string{from, to},
		curr.Unix(),
	)
	return redisErrIsNil(cmd)
}

func (queue *redisQueue) batchSize() int {
	prefetchCount := len(queue.deliveryChan)
	prefetchLimit := queue.prefetchLimit - prefetchCount
	// TODO: ignore ready count here and just return prefetchLimit?
	if readyCount := queue.ReadyCount(); readyCount < prefetchLimit {
		return readyCount
	}
	return prefetchLimit
}

// consumeBatch tries to read batchSize deliveries, returns true if any and all were consumed
func (queue *redisQueue) consumeBatch(batchSize int) bool {
	if batchSize == 0 {
		return false
	}

	for i := 0; i < batchSize; i++ {
		result := queue.redisClient.RPopLPush(context.Background(), queue.readyKey, queue.unackedKey)
		if redisErrIsNil(result) {
			// debug(fmt.Sprintf("rmq queue consumed last batch %s %d", queue, i)) // COMMENTOUT
			return false
		}

		// debug(fmt.Sprintf("consume %d/%d %s %s", i, batchSize, result.Val(), queue)) // COMMENTOUT
		queue.deliveryChan <- newDelivery(result.Val(), queue.unackedKey, queue.rejectedKey, queue.pushKey, queue.delayedKey, queue.redisClient)
	}

	// debug(fmt.Sprintf("rmq queue consumed batch %s %d", queue, batchSize)) // COMMENTOUT
	return true
}

func (queue *redisQueue) consumerConsume(consumer Consumer) {
	for delivery := range queue.deliveryChan {
		// debug(fmt.Sprintf("consumer consume %s %s", delivery, consumer)) // COMMENTOUT
		consumer.Consume(delivery)
	}
}

func (queue *redisQueue) consumerBatchConsume(batchSize int, timeout time.Duration, consumer BatchConsumer) {
	batch := []Delivery{}
	timer := time.NewTimer(timeout)
	stopTimer(timer) // timer not active yet

	for {
		select {
		case <-timer.C:
			// debug("batch timer fired") // COMMENTOUT
			// consume batch below

		case delivery, ok := <-queue.deliveryChan:
			if !ok {
				// debug("batch channel closed") // COMMENTOUT
				return
			}

			batch = append(batch, delivery)
			// debug(fmt.Sprintf("batch consume added delivery %d", len(batch))) // COMMENTOUT

			if len(batch) == 1 { // added first delivery
				timer.Reset(timeout) // set timer to fire
			}

			if len(batch) < batchSize {
				// debug(fmt.Sprintf("batch consume wait %d < %d", len(batch), batchSize)) // COMMENTOUT
				continue
			}

			// consume batch below
		}

		// debug(fmt.Sprintf("batch consume consume %d", len(batch))) // COMMENTOUT
		consumer.Consume(batch)

		batch = batch[:0] // reset batch
		stopTimer(timer)  // stop and drain the timer if it fired in between
	}
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}

	select {
	case <-timer.C:
	default:
	}
}

// return number of deleted list items
// https://www.redisgreen.net/blog/deleting-large-lists
func (queue *redisQueue) deleteRedisList(key string) int {
	llenResult := queue.redisClient.LLen(context.Background(), key)
	total := int(llenResult.Val())
	if total == 0 {
		return 0 // nothing to do
	}

	// delete elements without blocking
	for todo := total; todo > 0; todo -= purgeBatchSize {
		// minimum of purgeBatchSize and todo
		batchSize := purgeBatchSize
		if batchSize > todo {
			batchSize = todo
		}

		// remove one batch
		queue.redisClient.LTrim(context.Background(), key, 0, int64(-1-batchSize))
	}

	return total
}

func (queue *redisQueue) deleteRedisSortedSet(key string) int {
	zcountResult := queue.redisClient.ZCount(context.Background(), key, "-inf", "+inf")
	total := int(zcountResult.Val())
	if total == 0 {
		return 0 // nothing to do
	}

	// delete elements without blocking
	for todo := total; todo > 0; todo -= purgeBatchSize {
		// minimum of purgeBatchSize and todo
		batchSize := purgeBatchSize
		if batchSize > todo {
			batchSize = todo
		}

		// remove one batch
		queue.redisClient.ZRemRangeByRank(context.Background(), key, 0, int64(-1-batchSize))
	}

	return total
}

// redisErrIsNil returns false if there is no error, true if the result error is nil and panics if there's another error
func redisErrIsNil(result redis.Cmder) bool {
	switch result.Err() {
	case nil:
		return false
	case redis.Nil:
		return true
	default:
		log.Panicf("rmq redis error is not nil %s", result.Err())
		return false
	}
}

func debug(message string) {
	// log.Printf("rmq debug: %s", message) // COMMENTOUT
}

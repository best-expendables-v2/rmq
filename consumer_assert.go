package rmq

import (
	"time"
)

func assertUnacked(queue *redisQueue, v int) bool {
	for i := 0; i < 100; i++ {
		time.Sleep(100 * time.Millisecond)
		if queue.UnackedCount() == v {
			return true
		}
	}
	return false
}

func assertReady(queue *redisQueue, v int) bool {
	for i := 0; i < 100; i++ {
		time.Sleep(100 * time.Millisecond)
		if queue.ReadyCount() == v {
			return true
		}
	}
	return false
}

func assertDelayed(queue *redisQueue, v int) bool {
	for i := 0; i < 100; i++ {
		time.Sleep(100 * time.Millisecond)
		if queue.DelayedCount() == v {
			return true
		}
	}
	return false
}

func waitFor(payload string, consumer *TestConsumer) string {
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		if consumer == nil || consumer.LastDelivery == nil {
			continue
		}
		if consumer.LastDelivery.Payload() == payload {
			break
		}
	}
	return payload
}

func waitForAll(payloads []string, consumer *TestConsumer) []string {
	for _, pl := range payloads {
		a := waitFor(pl, consumer)
		debug(a)
	}
	return payloads
}

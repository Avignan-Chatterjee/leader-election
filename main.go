package main

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/clientv3"
)

var (
	requestTimeout = 10 * time.Second
	endpoints      = []string{
		"http://192.168.0.190:2389",
		"http://192.168.0.191:2389",
		"http://192.168.0.192:2389",
	}
	cli                 *clientv3.Client
	acquireLeadershipCh chan struct{}
	hostid              uuid.UUID
	leadershipLease     int64 = 4
	isLeader            bool
	leaseResp           *clientv3.LeaseGrantResponse
)

func main() {
	hostid = uuid.New()
	var err error
	cli, err = clientv3.New(clientv3.Config{
		Endpoints: endpoints,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close()
	acquireLeadershipCh = make(chan struct{})

	go watcher()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	_, err = cli.Put(ctx, "sample_key", "sample_value")
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	go acquirLeadership(hostid)
	acquireLeadershipCh <- struct{}{}
	select {}
}

func acquirLeadership(u uuid.UUID) {
	var err error
	halfLife := time.Duration(leadershipLease - 1)
	ticker := time.NewTicker(halfLife * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if isLeader {
				ka, kaerr := cli.KeepAliveOnce(context.TODO(), leaseResp.ID)
				if kaerr != nil {
					log.Printf("Failed to renew lease :: %v", kaerr)
				}
				log.Println("ttl:", ka.TTL)
			}
		case <-acquireLeadershipCh:
			log.Printf("Trying to acquire leadership\n")
			// Minimum lease TTL is 5-second
			leaseResp, err = cli.Grant(context.TODO(), leadershipLease)
			if err != nil {
				log.Printf("error getting lease :: %v", err)
				continue
			}

			ctxT, cancelT := context.WithTimeout(context.Background(), requestTimeout)
			txnResp, errT := cli.Txn(ctxT).
				// txn value comparisons are lexical
				//If(clientv3.Compare(clientv3.Value("leader"), "=", "")).
				If(clientv3.CreateRevision("leader")).
				// the "Then" runs, since "xyz" > "abc"
				Then(clientv3.OpPut("leader", u.String(), clientv3.WithLease(leaseResp.ID))).
				// the "Else" does not run
				//Else(clientv3.OpPut("txn_failed", "true")).
				Else().
				Commit()
			cancelT()
			if errT != nil {
				log.Printf("Failed transaction :: %v", err)
				continue
			}
			log.Printf("txn resp %v\n", txnResp.Succeeded)
			if !txnResp.Succeeded {
				isLeader = false
				if leaseResp != nil {
					_, err = cli.Revoke(context.TODO(), leaseResp.ID)
					if err != nil {
						log.Printf("Unused lease revoke error :: %v", err)
					}
					leaseResp = nil
				}
			}
		}
	}
}

func handleLostLeadership() {
	var err error
	// If some other member deleted the leader key then the actual
	// leader needs to cleanup the lease.
	if isLeader {
		// This makes sure that if the leadership is lost then the
		// unhandled lease of the previous leader does not
		// interfere with the new leader by cleaing up leadership
		// key.
		if leaseResp != nil {
			_, err = cli.Revoke(context.TODO(), leaseResp.ID)
			if err != nil {
				log.Printf("Unused lease revoke error :: %v", err)
			}
			leaseResp = nil
		}
	}
}

func watcher() {
	rch := cli.Watch(context.Background(), "leader")
	for wresp := range rch {
		for _, ev := range wresp.Events {
			log.Printf("%s %q : %q\n", ev.Type, ev.Kv.Key, ev.Kv.Value)
			if ev.Type == clientv3.EventTypeDelete {
				handleLostLeadership()
				acquireLeadershipCh <- struct{}{}
			} else if string(ev.Kv.Value) == hostid.String() {
				isLeader = true
				log.Printf("Successfully acquired leadership\n")
			}
		}
	}
}

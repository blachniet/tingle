package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/influxdata/influxdb/client/v2"
)

var (
	cfgRouterAddr   = getEnvDef("TINGLE_ROUTER_ADDR", "192.168.1.1:23")
	cfgRouterUser   = getEnvReq("TINGLE_ROUTER_USER")
	cfgRouterPwd    = getEnvReq("TINGLE_ROUTER_PWD")
	cfgInfluxDBHost = getEnvDef("TINGLE_INFLUXDB_HOST", "http://localhost:8086")
	cfgInfluxDBName = getEnvDef("TINGLE_INFLUXDB_NAME", "tingle")
)

func main() {
	log.WithFields(log.Fields{
		"routerAddr":   cfgRouterAddr,
		"routerUser":   cfgRouterUser,
		"influxDBHost": cfgInfluxDBHost,
		"influxDBName": cfgInfluxDBName,
	}).Info("Tingle start")

	// Attempt to establish connection
	var conn net.Conn
	for {
		var err error
		conn, err = connect(cfgRouterAddr, cfgRouterUser, cfgRouterPwd)
		if err != nil {
			log.WithError(err).Error("Error connecting to router. Retrying...")
			time.Sleep(10 * time.Second)
		} else {
			break
		}
	}
	defer conn.Close()

	// Connect to influx
	c, err := client.NewHTTPClient(client.HTTPConfig{
		Addr: cfgInfluxDBHost,
	})
	if err != nil {
		log.WithError(err).Fatal("Failed to connect to influx")
	}

	// Create the influx database if it doesn't already exist
	_, err = queryDB(c, cfgInfluxDBName, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", cfgInfluxDBName))
	if err != nil {
		log.WithError(err).Fatal("Error creating database")
	}

	// Start influx emission routine
	dpc := make(chan dataPoint, 100)
	defer close(dpc)
	emit(c, cfgInfluxDBName, dpc)

	// Start gathering
	for update := range gather(conn) {
		if update.Err != nil {
			log.WithError(update.Err).Error("Error during gathering")
		}
		if update.DP != nil {
			dpc <- *update.DP
		}
	}

	// Wait for termination signal
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Info("Stopping...")
}

func connect(addr, username, password string) (net.Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	err = login(conn, username, password)
	if err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

func login(conn net.Conn, username, password string) error {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString(':')
	if err != nil {
		return err
	} else if !strings.HasSuffix(strings.ToLower(line), "login:") {
		return fmt.Errorf("Unexpected response from server, expected login request")
	}
	fmt.Fprintf(conn, username+"\n")

	line, err = reader.ReadString(':')
	if err != nil {
		return err
	} else if !strings.HasSuffix(strings.ToLower(line), "password:") {
		return fmt.Errorf("Unexpected response from server, expected password request")
	}
	fmt.Fprintf(conn, password+"\n")
	return nil
}

func gather(conn net.Conn) <-chan gatherUpdate {
	upchan := make(chan gatherUpdate)
	go func() {
		defer close(upchan)
		ifaceStates := map[string]*ifaceState{}

		fmt.Fprintf(conn, "while true; do ls /sys/class/net -1 | while read iface; do echo \"TNGL,${iface},$(cat /sys/class/net/${iface}/statistics/rx_bytes),$(cat /sys/class/net/${iface}/statistics/tx_bytes)\"; done; sleep 10; done\n")
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			txt := scanner.Text()
			parts := strings.Split(txt, ",")
			if len(parts) > 0 && parts[0] == "TNGL" {
				iface := parts[1]
				rxBytesLt, err := strconv.Atoi(parts[2])
				if err != nil {
					upchan <- gatherUpdate{err, nil}
				}
				txBytesLt, err := strconv.Atoi(parts[3])
				if err != nil {
					upchan <- gatherUpdate{err, nil}
				}

				if s, ok := ifaceStates[iface]; ok {
					rxBytes := rxBytesLt - s.RXBytesLifetime
					txBytes := txBytesLt - s.TXBytesLifetime
					if rxBytes >= 0 && txBytes >= 0 {
						upchan <- gatherUpdate{nil, &dataPoint{
							Interface:       iface,
							RXBytesLifetime: rxBytesLt,
							TXBytesLifetime: txBytesLt,
							RXBytes:         rxBytes,
							TXBytes:         txBytes,
							Timestamp:       time.Now(),
						}}
					} else {
						log.WithField("iface", iface).Info("Lifetime bytes reset")
					}
					s.RXBytesLifetime = rxBytesLt
					s.TXBytesLifetime = txBytesLt
				} else {
					ifaceStates[iface] = &ifaceState{rxBytesLt, txBytesLt}
				}
			}
		}
		upchan <- gatherUpdate{scanner.Err(), nil}
	}()
	return upchan
}

func emit(c client.Client, db string, dpc <-chan dataPoint) {
	go func() {
		bp, err := client.NewBatchPoints(client.BatchPointsConfig{
			Database:  db,
			Precision: "s",
		})
		if err != nil {
			log.WithError(err).Fatal("Error creating batch points")
		}

		ticker := time.NewTicker(time.Second * 2)
		for {
			select {
			case <-ticker.C:
				pointCount := len(bp.Points())
				if pointCount > 0 {
					log.WithField("count", pointCount).Info("Sending batch to InfluxDB")
					err := c.Write(bp)
					if err != nil {
						log.WithError(err).Error("Error writing data to influxdb")
					}
					bp, err = client.NewBatchPoints(client.BatchPointsConfig{
						Database:  db,
						Precision: "s",
					})
					if err != nil {
						log.WithError(err).Fatal("Error creating batch points")
					}
				}
			case dp := <-dpc:
				tags := map[string]string{"iface": dp.Interface}
				fields := map[string]interface{}{
					"rx_bytes_lt": dp.RXBytesLifetime,
					"tx_bytes_lt": dp.TXBytesLifetime,
					"xx_bytes_lt": dp.RXBytesLifetime + dp.TXBytesLifetime,
					"rx_bytes":    dp.RXBytes,
					"tx_bytes":    dp.TXBytes,
					"xx_bytes":    dp.RXBytes + dp.TXBytes,
				}
				pt, err := client.NewPoint("net_usage", tags, fields, dp.Timestamp)
				if err != nil {
					log.WithError(err).Error("Error creating point")
				} else {
					bp.AddPoint(pt)
				}
				log.WithField("point", pt).Info("InfluxDB point built")
			}
		}
	}()
}

func queryDB(clnt client.Client, db, cmd string) (res []client.Result, err error) {
	q := client.Query{
		Command:  cmd,
		Database: db,
	}
	if response, err := clnt.Query(q); err == nil {
		if response.Error() != nil {
			return res, response.Error()
		}
		res = response.Results
	} else {
		return res, err
	}
	return res, nil
}

type gatherUpdate struct {
	Err error
	DP  *dataPoint
}

type dataPoint struct {
	Interface       string
	RXBytesLifetime int
	TXBytesLifetime int
	RXBytes         int
	TXBytes         int
	Timestamp       time.Time
}

type ifaceState struct {
	RXBytesLifetime int
	TXBytesLifetime int
}

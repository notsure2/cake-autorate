package main

import (
	"flag"
	"fmt"
	"github.com/go-ping/ping"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type PingReply struct {
	Packet      *ping.Packet
	PacketsLost bool
}

func main() {
	uploadInterface := flag.String("uploadInterface", "", "upload interface")
	downloadInterface := flag.String("downloadInterface", "", "download interface (usually ifbX)")
	maxUploadRateKilobits := flag.Uint64("maxUploadRate", 0, "maximum upload rate in kilobits per second")
	minUploadRateKilobits := flag.Uint64("minUploadRate", 0, "minimum upload rate in kilobits per second (default: 20% of max)")
	maxDownloadRateKilobits := flag.Uint64("maxDownloadRate", 0, "maximum download rate in kilobits per second")
	minDownloadRateKilobits := flag.Uint64("minDownloadRate", 0, "minimum download rate in kilobits per second (default: 20% of max)")
	tickDuration := flag.Duration("tickDuration", 500*time.Millisecond, "tick duration")
	rttIncreaseFactor := flag.Float64("rttIncreaseFactor", 0.001, "how rapidly baseline RTT is allowed to increase")
	rttDecreaseFactor := flag.Float64("rttDecreaseFactor", 0.9, "how rapidly baseline RTT is allowed to decrease")
	rateAdjustOnRttSpikeFactor := flag.Float64("rateAdjustOnRttSpikeFactor", 0.05, "how rapidly to reduce bandwidth upon detection of bufferbloat")
	rateLoadIncreaseFactor := flag.Float64("rateLoadIncreaseFactor", 0.0125, "how rapidly to increase bandwidth upon high load detected")
	rateLoadDecreaseFactor := flag.Float64("rateLoadDecreaseFactor", 0, "how rapidly to decrease bandwidth upon low load detected")
	loadThreshold := flag.Uint64("loadThreshold", 50, "% of currently set bandwidth for detecting high load")
	rttSpikeThresholdMs := flag.Uint64("rttSpikeThresholdMs", 15, "increase from baseline RTT for detection of bufferbloat in ms")
	reflectorHost := flag.String("reflectorHost", "1.1.1.1", "host to use for measuring ping")
	flag.Parse()

	if *uploadInterface == "" {
		fmt.Println("upload interface must be specified.")
		os.Exit(1)
	}

	if *downloadInterface == "" {
		fmt.Println("download interface must be specified.")
		os.Exit(1)
	}

	if *maxUploadRateKilobits == 0 {
		fmt.Println("max upload rate must be specified.")
		os.Exit(1)
	}

	if *minUploadRateKilobits == 0 {
		*minUploadRateKilobits = *maxUploadRateKilobits / 5
	}

	if *minUploadRateKilobits > *maxUploadRateKilobits {
		fmt.Println("min upload rate must be less than the max")
		os.Exit(1)
	}

	if *maxDownloadRateKilobits == 0 {
		fmt.Println("max download rate must be specified.")
		os.Exit(1)
	}

	if *minDownloadRateKilobits == 0 {
		*minDownloadRateKilobits = *maxDownloadRateKilobits / 5
	}

	if *minDownloadRateKilobits > *maxDownloadRateKilobits {
		fmt.Println("min download rate must be less than the max")
		os.Exit(1)
	}

	if *tickDuration == 0 {
		fmt.Println("tick duration must be a positive duration in a format like 500ms")
		os.Exit(1)
	}

	if *rttIncreaseFactor <= 0 {
		fmt.Println("rtt increase factor must be more than 0")
		os.Exit(1)
	}

	if *rttDecreaseFactor <= 0 {
		fmt.Println("rtt decrease factor must be more than 0")
		os.Exit(1)
	}

	if *rateAdjustOnRttSpikeFactor <= 0 {
		fmt.Println("rate adjust on rtt spike factor must be more than 0")
		os.Exit(1)
	}

	if *rateLoadIncreaseFactor <= 0 {
		fmt.Println("rate load increase factor must be more than 0")
		os.Exit(1)
	}

	if *rateLoadDecreaseFactor < 0 {
		fmt.Println("rate load decrease factor must be 0 or more")
		os.Exit(1)
	}

	if *loadThreshold <= 0 || *loadThreshold > 80 {
		fmt.Println("load threshold must not be zero or greater than 80")
		os.Exit(1)
	}

	if *rttSpikeThresholdMs < 15 {
		fmt.Println("rtt spike threshold must be 15 or greater")
		os.Exit(1)
	}

	var rxBytesPath string
	if strings.HasPrefix(*downloadInterface, "veth") || strings.HasPrefix(*uploadInterface, "ifb") {
		rxBytesPath = fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", *downloadInterface)
	} else {
		rxBytesPath = fmt.Sprintf("/sys/class/net/%s/statistics/rx_bytes", *downloadInterface)
	}

	var txBytesPath string
	if strings.HasPrefix(*uploadInterface, "veth") || strings.HasPrefix(*uploadInterface, "ifb") {
		txBytesPath = fmt.Sprintf("/sys/class/net/%s/statistics/rx_bytes", *downloadInterface)
	} else {
		txBytesPath = fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", *downloadInterface)
	}

	log.Printf(
		"uploadInterface: %s (max: %d kbps - min: %d kbps)\n",
		*uploadInterface,
		*maxUploadRateKilobits,
		*minUploadRateKilobits)

	log.Printf(
		"downloadInterface: %s (max: %d kbps - min: %d kbps)\n",
		*downloadInterface,
		*maxDownloadRateKilobits,
		*minDownloadRateKilobits)

	log.Printf("rxBytesPath: %s\n", rxBytesPath)
	log.Printf("txBytesPath: %s\n", txBytesPath)
	log.Printf("reflectorHost: %s\n", *reflectorHost)

	pinger, err := ping.NewPinger(*reflectorHost)
	if err != nil {
		panic(err)
	}

	pinger.Count = 0
	pinger.Interval = *tickDuration / 4
	pinger.RecordRtts = false
	pinger.SetPrivileged(true)

	pingerCancelSignal := make(chan os.Signal, 1)
	signal.Notify(pingerCancelSignal, os.Interrupt)
	go func() {
		for range pingerCancelSignal {
			pinger.Stop()
		}
	}()

	lastSeqReceived := 0
	var lastPingReply atomic.Value

	pinger.OnRecv = func(packet *ping.Packet) {
		reply := PingReply{
			Packet:      packet,
			PacketsLost: packet.Seq > lastSeqReceived+1,
		}
		lastSeqReceived = packet.Seq
		lastPingReply.Store(&reply)
	}

	pingerExitChannel := make(chan error)
	go func() {
		err := pinger.Run()
		pingerExitChannel <- err
	}()

	tickerCancelSignal := make(chan os.Signal, 1)
	signal.Notify(tickerCancelSignal, os.Interrupt)
	tickerExitChannel := make(chan *error)
	go func() {
		// Ensure we received a few ping replies to establish the baseline
		time.Sleep(*tickDuration * 2)

		pingReply := lastPingReply.Load().(*PingReply)
		baselineRtt := pingReply.Packet.Rtt
		downloadRateKilobits := *maxDownloadRateKilobits / 2
		uploadRateKilobits := *maxUploadRateKilobits / 2
		setCakeRate(*downloadInterface, downloadRateKilobits)
		setCakeRate(*uploadInterface, uploadRateKilobits)

		lastRxBytes := readSysFsBytes(rxBytesPath)
		lastTxBytes := readSysFsBytes(txBytesPath)
		lastBytesReadTime := time.Now()

		ticker := time.NewTicker(*tickDuration)

		for {
			breakLoop := false
			select {
			case <-tickerCancelSignal:
				breakLoop = true
				break

			case <-ticker.C:
				pingReply = lastPingReply.Load().(*PingReply)
				newRtt := pingReply.Packet.Rtt

				rttDelta := pingReply.Packet.Rtt - baselineRtt
				rttFactor := *rttIncreaseFactor
				if rttDelta < 0 {
					rttFactor = *rttDecreaseFactor
				}
				newBaselineRttMs := int64(((1 - rttFactor) * float64(baselineRtt.Milliseconds())) + (rttFactor * float64(newRtt.Milliseconds())))
				baselineRtt = time.Duration(newBaselineRttMs) * time.Millisecond

				rxBytes := readSysFsBytes(rxBytesPath)
				txBytes := readSysFsBytes(txBytesPath)
				bytesReadTime := time.Now()
				rxBytesDelta := rxBytes - lastRxBytes
				if rxBytesDelta < 0 {
					rxBytesDelta += 2 ^ 64
				}
				txBytesDelta := txBytes - lastTxBytes
				if txBytesDelta < 0 {
					txBytesDelta += 2 ^ 64
				}
				timeDelta := bytesReadTime.Sub(lastBytesReadTime)

				rxLoad := uint64((float64(rxBytesDelta*8/1000) / timeDelta.Seconds() / float64(downloadRateKilobits)) * 100)
				txLoad := uint64((float64(txBytesDelta*8/1000) / timeDelta.Seconds() / float64(uploadRateKilobits)) * 100)

				nextUploadRateKilobits := uploadRateKilobits
				nextDownloadRateKilobits := downloadRateKilobits
				if pingReply.PacketsLost || uint64(rttDelta.Milliseconds()) >= *rttSpikeThresholdMs {
					nextDownloadRateKilobits = downloadRateKilobits - uint64(*rateAdjustOnRttSpikeFactor*float64(*maxDownloadRateKilobits-*minDownloadRateKilobits))
					nextUploadRateKilobits = uploadRateKilobits - uint64(*rateAdjustOnRttSpikeFactor*float64(*maxUploadRateKilobits-*minUploadRateKilobits))
				} else {
					if rxLoad >= *loadThreshold {
						nextDownloadRateKilobits = downloadRateKilobits + uint64(*rateLoadIncreaseFactor*float64(*maxDownloadRateKilobits-*minDownloadRateKilobits))
					} else {
						nextDownloadRateKilobits = downloadRateKilobits - uint64(*rateLoadDecreaseFactor*float64(*maxDownloadRateKilobits-*minDownloadRateKilobits))
					}

					if txLoad >= *loadThreshold {
						nextUploadRateKilobits = uploadRateKilobits + uint64(*rateLoadIncreaseFactor*float64(*maxUploadRateKilobits-*minUploadRateKilobits))
					} else {
						nextUploadRateKilobits = uploadRateKilobits - uint64(*rateLoadDecreaseFactor*float64(*maxUploadRateKilobits-*minUploadRateKilobits))
					}
				}

				if nextDownloadRateKilobits < *minDownloadRateKilobits {
					nextDownloadRateKilobits = *minDownloadRateKilobits
				}

				if nextDownloadRateKilobits > *maxDownloadRateKilobits {
					nextDownloadRateKilobits = *maxDownloadRateKilobits
				}

				if nextUploadRateKilobits < *minUploadRateKilobits {
					nextUploadRateKilobits = *minUploadRateKilobits
				}

				if nextUploadRateKilobits > *maxUploadRateKilobits {
					nextUploadRateKilobits = *maxUploadRateKilobits
				}

				downloadRateKilobits = nextDownloadRateKilobits
				uploadRateKilobits = nextUploadRateKilobits
				lastRxBytes = rxBytes
				lastTxBytes = txBytes
				lastBytesReadTime = bytesReadTime

				setCakeRate(*downloadInterface, downloadRateKilobits)
				setCakeRate(*uploadInterface, uploadRateKilobits)

				log.Printf(
					"rxLoad: %d; txLoad: %d; baselineRtt: %s; newRtt: %s; rttDelta: %s; dl: %dKbit; ul: %dKbit;\n",
					rxLoad,
					txLoad,
					baselineRtt,
					newRtt,
					rttDelta,
					nextDownloadRateKilobits,
					nextUploadRateKilobits)
			}

			if breakLoop {
				break
			}
		}
		tickerExitChannel <- nil
	}()

	errorExit := false

	pingerErr := <-pingerExitChannel
	if pingerErr != nil {
		log.Printf("Pinger error: %s\n", err)
		errorExit = true
	}

	tickerErr := <-tickerExitChannel
	if tickerErr != nil {
		log.Printf("Ticker error: %s\n", err)
		errorExit = true
	}

	if errorExit {
		os.Exit(1)
	}

	statistics := pinger.Statistics()
	if statistics != nil {
		log.Printf("Max RTT: %s\n", statistics.MaxRtt)
	}
}

func setCakeRate(interfaceName string, kilobitsPerSecond uint64) {
	cmd := exec.Command(fmt.Sprintf("tc qdisc change root dev %s cake bandwidth %dKbit", interfaceName, kilobitsPerSecond))
	output, err := cmd.Output()
	if err != nil {
		log.Printf("Failed to set qdisc rate %d for %s: %s - %s\n", kilobitsPerSecond, interfaceName, err, output)
	}
}

func readSysFsBytes(path string) uint64 {
	bytes, err := ioutil.ReadFile(path) // just pass the file name
	if err != nil {
		log.Printf("Failed to read stats file %s\n", path)
		return 0
	}

	str := string(bytes[:len(bytes)-1])
	result, err := strconv.ParseUint(str, 10, 64)
	if err != nil {
		log.Printf("Failed to read stats file %s result '%s'\n", path, str)
		return 0
	}
	return result
}

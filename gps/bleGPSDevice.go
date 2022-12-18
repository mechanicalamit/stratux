/*
	Copyright (c) 2021 R. van Twisk
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	ais.go: Routines for reading AIS traffic
*/

package gps

import (
	"log"
	"strings"
	"sync"

	"time"

	"github.com/b3nn0/stratux/v2/common"
	cmap "github.com/orcaman/concurrent-map/v2"
	"tinygo.org/x/bluetooth"
)

// Holds information about a device that is currently within a list of discovered devices
type discoveredDeviceInfo struct {
	Connected bool
	MAC       string
	name      string
	Allowed   bool
}

// Hold's information about a device that has beeing scanned
type scanInfoResult struct {
	MAC  string
	name string
}

type BleGPSDevice struct {
	adapter              bluetooth.Adapter
	discoveredDeviceList cmap.ConcurrentMap[*discoveredDeviceInfo]
	eh                   *common.ExitHelper
	rxMessageCh          chan<- RXMessage
	scanMutex			 *sync.Mutex
	scanInfoCh			 chan scanInfoResult
}

func NewBleGPSDevice(rxMessageCh chan<- RXMessage) BleGPSDevice {
	return BleGPSDevice{
		adapter:              *bluetooth.DefaultAdapter,
		discoveredDeviceList: cmap.New[*discoveredDeviceInfo](),
		eh:                   common.NewExitHelper(),
		rxMessageCh:          rxMessageCh,
		scanMutex:            &sync.Mutex{},
		scanInfoCh:           make(chan scanInfoResult, 5),
	}
}

var (
	HM_10_CONF, _ = bluetooth.ParseUUID("0000ffe0-0000-1000-8000-00805f9b34fb")
	BLE_RX, _     = bluetooth.ParseUUID("0000ffe1-0000-1000-8000-00805f9b34fb")
)

// startScanningBluetoothLEDevices will scan for any nearby devices add notifies them on the scanInfoResult for any found devices
// Sometimes we get CRC errors from a attached GPS device that will look like this :GPS error. Invalid NMEA string: Checksum failed. Calculated 0X7F; expected 0X68 $GPGSV,3,1,10,01,15,256,,08,72,282,,10,50*68
func (b *BleGPSDevice) startScanningBluetoothLEDevices(leh *common.ExitHelper) {
	leh.Add()
	defer func() {
		leh.Done()
	}()

	// Scan is blocking, keep that in mind
	err := b.adapter.Scan(
		func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
			// This test for exit needed otherwise if there is no bluetooth device at all, we keep scanning forever
			if leh.IsExit() {
				b.adapter.StopScan()
			} else if result.AdvertisementPayload.HasServiceUUID(HM_10_CONF) && 
				result.Address != nil &&
				strings.TrimSpace(result.LocalName()) != "" {
				// LocalName is the (complete or shortened) local name of the device.
				// Please note that many devices do not broadcast a local name, but may
				// broadcast other data (e.g. manufacturer data or service UUIDs) with which
				// they may be identified.
				b.scanInfoCh <- scanInfoResult{result.Address.String(), result.LocalName()}
			}
		})

	if err != nil {
		log.Printf("Error from ble scanner: %s", err.Error())
	}
}

/**
Coonect to our bluetooth device and listen on the RX channel for NMEA sentences
**/
func (b *BleGPSDevice) rxListener(discoveredDeviceInfo discoveredDeviceInfo) error {
	b.eh.Add()
	defer b.eh.Done()

	address, _ := bluetooth.ParseMAC(discoveredDeviceInfo.MAC)
	btAddress := bluetooth.MACAddress{MAC: address}

	// Connect to device
	device, err := b.adapter.Connect(bluetooth.Address{MACAddress: btAddress}, bluetooth.ConnectionParams{})
	if err != nil {
		return err
	}
	defer device.Disconnect()

	// Find the service
	services, err := device.DiscoverServices([]bluetooth.UUID{HM_10_CONF})
	if err != nil {
		return err
	}
	service := services[0]

	// Get the two characteristics present in this service.
	chars, err := service.DiscoverCharacteristics([]bluetooth.UUID{BLE_RX})
	if err != nil {
		return err
	}

	log.Printf("Connected to : %s", discoveredDeviceInfo.name)

	// Create a TX Channel and send a connect discovery
	TXChannel := make(chan []byte, 1)

	GetServiceDiscovery().Send(DiscoveredDevice{
		Name:      discoveredDeviceInfo.name,
		Content:   CONTENT_TX_CHANNEL | CONTENT_CONNECTED,
		TXChannel: TXChannel,
		Connected: true,
	})
	defer GetServiceDiscovery().Connected(discoveredDeviceInfo.name, false)

	// Mutex + condition to sync the write/read routines
	mutex := sync.Mutex{}
	condition := sync.NewCond(&mutex)
	defer condition.Signal() // This to give the below go routine a chance to exit when it's waiting for the condition

	// variables for the NMEA parser
	var receivedData []byte

	tx := chars[0]
	go func() {
		const MAX_NMEA_LENGTH = 79
		var charPosition int
		byteArray := [MAX_NMEA_LENGTH + 1]byte{} // One extra for zero termination
		sentenceStarted := false
		for {
			mutex.Lock()
			condition.Wait()
			if b.eh.IsExit() {
				log.Printf("Exiting rxListener")
				return
			}
			for i := 0; i < len(receivedData); i++ {
				c := receivedData[i]
				// Within NMEA sentence?
				if sentenceStarted &&
					c >= 0x20 && c <= 0x7e &&
					charPosition < MAX_NMEA_LENGTH {
					byteArray[charPosition] = c
					charPosition++
				}

				// End of a NMEA sentence
				if c == 0x0d && sentenceStarted && charPosition < MAX_NMEA_LENGTH {
					sentenceStarted = false
					thisOne := strings.Clone(string(byteArray[0:charPosition]))
					select {
					case b.rxMessageCh <- RXMessage{
						Name:     discoveredDeviceInfo.name,
						NmeaLine: thisOne,
					}:
					default:
						log.Printf("BLE rxMessageCh Full")
					}
				}

				// Start of a new NMEA sentence
				if c == '$' {
					sentenceStarted = true
					byteArray[0] = c
					charPosition = 1
				}
			}
			receivedData = receivedData[:0]
			mutex.Unlock()
		}
	}()

	// Callback from the enable notification function
	// This might (depends on underlaying implementation) run in a interrupt where we cannot allocate any heap
	// we use a mutex with signal to copy the received dataset into a existing byte array for further processing
	watchdogTimer := common.NewWatchDog(1000 * time.Millisecond)
	defer watchdogTimer.Stop()

	// Listen to bluetooth messages
	enaNotifyErr := tx.EnableNotifications(func(value []byte) {
		// Reset the watchdog
		watchdogTimer.Poke()

		// Copy received data
		mutex.Lock()
		receivedData = append(receivedData, value...)
		condition.Signal()
		mutex.Unlock()
	})

	if enaNotifyErr != nil {
		return enaNotifyErr
	}

	select {
	case <-b.eh.C:
		return nil
	case <-watchdogTimer.C:
		return nil
	}
}

/**
connectionMonitor monitors the list discoveredDeviceList for disconnected devices and reconnects them again
*/
func (b *BleGPSDevice) connectionMonitor() {
	b.eh.Add()
	defer b.eh.Done()

	ticker := time.NewTicker(15000 * time.Millisecond)
	for {
		select {
		case <-b.eh.C:
			return
		case <-ticker.C:
			for entry := range b.discoveredDeviceList.IterBuffered() {
				info := entry.Val

				// When the device is not connected, we attemt to connect it again
				if !info.Connected && info.Allowed {
					info.Connected = true
					go func() {
						// Attempt to connect to a bluetooth device
						err := b.rxListener(*info)
						if err != nil {
							// log.Printf("BLE device error : %s : %s", info.name, err.Error())
						} else {
							log.Printf("BLE device was finished %s", info.name)
						}
						info.Connected = false
					}()
				}
			}
		}
	}
}

func (b *BleGPSDevice) Stop() {
	b.eh.Exit()
}

func (b *BleGPSDevice) setConfig( data map[string]interface{}) {
	// loop over map to create discoery
	for key, value := range data {
		m := value.(map[string]interface{})
 		b.discoveredDeviceList.SetIfAbsent(m["MAC"].(string), &discoveredDeviceInfo{
			Connected: false,
			MAC: m["MAC"].(string), 
			Allowed: true,
			name: key})

			GetServiceDiscovery().Send(DiscoveredDevice{
				Name:             key,
				Content:          CONTENT_TYPE | CONTENT_SOURCE | CONTENT_OFFSET_PPS,
				GPSDetectedType:  GPS_TYPE_BLUETOOTH,
				GPSSource:        GPS_SOURCE_BLUETOOTH,
				GPSTimeOffsetPPS: 200 * time.Millisecond,
			})			
	}
}

func (b *BleGPSDevice) GetConfig( ) map[string]interface{} {
	data := make(map[string]interface{})
	for entry := range b.discoveredDeviceList.IterBuffered() {
		if (entry.Val.Allowed) {
			data[entry.Val.MAC] = entry.Val
		}
	}
	return data
}

func (b *BleGPSDevice) Scan(leh *common.ExitHelper) {	
	log.Printf("Start scanning Bluetooth LE devices")
	b.startScanningBluetoothLEDevices(leh)
	log.Printf("Stop scanning Bluetooth LE devices")
}

func (b *BleGPSDevice) Run(deviceList map[string]interface{}) {
	b.eh.Add()
	defer b.eh.Done()

	if err := b.adapter.Enable(); err != nil {
		log.Printf("Failed to enable bluetooth LE adapter : %s", err.Error())
		return
	}
	b.setConfig(deviceList)
	go b.connectionMonitor()

	for {
		select {
		case <-b.eh.C:
			return
		case address := <-b.scanInfoCh:
			// Only allow names we see in our list in our allowed list
			added := b.discoveredDeviceList.SetIfAbsent(address.MAC, &discoveredDeviceInfo{Connected: false, Allowed: false, MAC: address.MAC, name: address.name})
			if added {
				log.Printf("BLE device %s added", address.name)

				GetServiceDiscovery().Send(DiscoveredDevice{
					Name:             address.name,
					MAC:			  address.MAC,
					Content:          CONTENT_TYPE | CONTENT_SOURCE | CONTENT_OFFSET_PPS,
					GPSDetectedType:  GPS_TYPE_BLUETOOTH,
					GPSSource:        GPS_SOURCE_BLUETOOTH,
					GPSTimeOffsetPPS: 200 * time.Millisecond,
				})
			}
		}
	}
}

/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	gps.go: GPS functions, GPS init, AHRS status messages, other external sensor monitoring.
*/

package serialGPSDevice

import (
	"errors"
	"log"
	"strings"
	"time"

	"bufio"

	"github.com/tarm/serial"

	"os"

	"github.com/b3nn0/stratux/v2/common"
	"github.com/b3nn0/stratux/v2/nmea"
)

type GlobalSettings struct {
	DEBUG bool
}
var globalSettings GlobalSettings

type SerialGPSDevice interface {
	Listen()
}

type serialGPSDevice struct {
	gpsTimeOffsetPpsMs time.Duration
	
	serialConfig *serial.Config
	serialPort *serial.Port
	
	readyToInitGPS bool //TODO: replace with channel control to terminate goroutine when complete
		
	ognTrackerConfigured bool

	GPS_detected_type uint
	GPS_connected bool
}

func New() SerialGPSDevice {
	m := serialGPSDevice {
		gpsTimeOffsetPpsMs: 100.0 * time.Millisecond,

		serialConfig: nil,
		serialPort: nil,

		readyToInitGPS: false,

		ognTrackerConfigured: false,

		GPS_detected_type: 0x00,
		GPS_connected: false}
	return m
}

/*
u-blox5_Referenzmanual.pdf
Platform settings
Airborne <2g Recommended for typical airborne environment. No 2D position fixes supported.
p.91 - CFG-MSG
Navigation/Measurement Rate Settings
Header 0xB5 0x62
ID 0x06 0x08
0x0064 (100 ms)
0x0001
0x0001 (GPS time)
{0xB5, 0x62, 0x06, 0x08, 0x00, 0x64, 0x00, 0x01, 0x00, 0x01}
p.109 CFG-NAV5 (0x06 0x24)
Poll Navigation Engine Settings
*/

/*
	chksumUBX()
		returns the two-byte Fletcher algorithm checksum of byte array msg.
		This is used in configuration messages for the u-blox GPS. See p. 97 of the
		u-blox M8 Receiver Description.
*/

func chksumUBX(msg []byte) []byte {
	ret := make([]byte, 2)
	for i := 0; i < len(msg); i++ {
		ret[0] = ret[0] + msg[i]
		ret[1] = ret[1] + ret[0]
	}
	return ret
}

/*
	makeUBXCFG()
		creates a UBX-formatted package consisting of two sync characters,
		class, ID, payload length in bytes (2-byte little endian), payload, and checksum.
		See p. 95 of the u-blox M8 Receiver Description.
*/
func makeUBXCFG(class, id byte, msglen uint16, msg []byte) []byte {
	ret := make([]byte, 6)
	ret[0] = 0xB5
	ret[1] = 0x62
	ret[2] = class
	ret[3] = id
	ret[4] = byte(msglen & 0xFF)
	ret[5] = byte((msglen >> 8) & 0xFF)
	ret = append(ret, msg...)
	chk := chksumUBX(ret[2:])
	ret = append(ret, chk[0])
	ret = append(ret, chk[1])
	return ret
}

func (s serialGPSDevice) initGPSSerial() bool {
	var device string
	if (s.GPS_detected_type & 0x0f) == common.GPS_TYPE_NETWORK {
		return true
	}
	// Possible baud rates for this device. We will try to auto detect the correct one
	baudrates := []int{int(9600)}
	isSirfIV := bool(false)
	s.ognTrackerConfigured = false
	s.GPS_detected_type = 0 // reset detected type on each initialization

	if _, err := os.Stat("/dev/ublox9"); err == nil { // u-blox 8 (RY83xAI over USB).
		device = "/dev/ublox9"
		s.GPS_detected_type = common.GPS_TYPE_UBX9
	} else if _, err := os.Stat("/dev/ublox8"); err == nil { // u-blox 8 (RY83xAI or GPYes 2.0).
		device = "/dev/ublox8"
		s.GPS_detected_type = common.GPS_TYPE_UBX8
		s.gpsTimeOffsetPpsMs = 80 * time.Millisecond // Ublox 8 seems to have higher delay
	} else if _, err := os.Stat("/dev/ublox7"); err == nil { // u-blox 7 (VK-172, VK-162 Rev 2, GPYes, RY725AI over USB).
		device = "/dev/ublox7"
		s.GPS_detected_type = common.GPS_TYPE_UBX7
	} else if _, err := os.Stat("/dev/ublox6"); err == nil { // u-blox 6 (VK-162 Rev 1).
		device = "/dev/ublox6"
		s.GPS_detected_type = common.GPS_TYPE_UBX6
	} else if _, err := os.Stat("/dev/prolific0"); err == nil { // Assume it's a BU-353-S4 SIRF IV.
		//TODO: Check a "serialout" flag and/or deal with multiple prolific devices.
		isSirfIV = true
		// default to 4800 for SiRFStar config port, we then change and detect it with 38400.
		// We also try 9600 just in case this is something else, as this is the most popular value
		baudrates = []int{4800, 38400, 9600}
		device = "/dev/prolific0"
		s.GPS_detected_type = common.GPS_TYPE_PROLIFIC
	} else if _, err := os.Stat("/dev/serialin"); err == nil {
		device = "/dev/serialin"
		s.GPS_detected_type = common.GPS_TYPE_SERIAL
		// OGN Tracker uses 115200, SoftRF 38400
		baudrates = []int{115200, 38400, 9600}
 	} else if _, err := os.Stat("/dev/softrf_dongle"); err == nil {
		device = "/dev/softrf_dongle"
		s.GPS_detected_type = common.GPS_TYPE_SOFTRF_DONGLE
		baudrates[0] = 115200
 	} else if _, err := os.Stat("/dev/ttyAMA0"); err == nil { // ttyAMA0 is PL011 UART (GPIO pins 8 and 10) on all RPi.
		device = "/dev/ttyAMA0"
		s.GPS_detected_type = common.GPS_TYPE_UART
		// UART connected u-blox GPS @ 10Hz update rate need 115200, 38400 and 9600 just as fallback
		baudrates = []int{115200, 38400, 9600}
	} else {
		if globalSettings.DEBUG {
			log.Printf("No GPS device found.\n")
		}
		return false
	}
	if globalSettings.DEBUG {
		log.Printf("Using %s for GPS\n", device)
	}

	// Open port at default baud for config.
	s.serialConfig = &serial.Config{Name: device, Baud: baudrates[0]}
	p, err := serial.OpenPort(s.serialConfig)
	if err != nil {
		log.Printf("serial port err: %s\n", err.Error())
		return false
	}

	if isSirfIV {
		log.Printf("Using SiRFIV config.\n")

		// Enable 5Hz. (To switch back to 1Hz: $PSRF103,00,7,00,0*22)
		p.Write(nmea.MakeNMEACmd("PSRF103,00,6,00,0"))
		// Enable GGA.
		p.Write(nmea.MakeNMEACmd("PSRF103,00,00,01,01"))
		// Enable GSA.
		p.Write(nmea.MakeNMEACmd("PSRF103,02,00,01,01"))
		// Enable RMC.
		p.Write(nmea.MakeNMEACmd("PSRF103,04,00,01,01"))
		// Enable VTG.
		p.Write(nmea.MakeNMEACmd("PSRF103,05,00,01,01"))
		// Enable GSV (once every 5 position updates)
		p.Write(nmea.MakeNMEACmd("PSRF103,03,00,05,01"))
		// Enable 38400 baud.
		p.Write(nmea.MakeNMEACmd("PSRF100,1,38400,8,1,0"))

		if globalSettings.DEBUG {
			log.Printf("Finished writing SiRF GPS config to %s. Opening port to test connection.\n", device)
		}
	} else if s.GPS_detected_type == common.GPS_TYPE_UART {
		// UBX-CFG-VALSET for u-blox M10S
		// RAM Layer configuration message
		// NMEA 4.0, NMEA extended svnumbering, dynamic model 7, AssistNow Autonomous, GPS+GAL+BDS+SBAS, 10Hz update rate, disable GLL
		payloadRAM := []byte{0xB5, 0x62, 0x06, 0x8A, 0x28, 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x23, 0x10, 0x01, 0x21, 0x00, 0x11, 0x20, 0x07, 0x01, 0x00, 0x21, 0x30, 0x64, 0x00, 0x22, 0x00, 0x31, 0x10, 0x01, 0xCA, 0x00, 0x91, 0x20, 0x00, 0x01, 0x00, 0x93, 0x20, 0x28, 0x07, 0x00, 0x93, 0x20, 0x01, 0x74, 0xCE}
		p.Write(payloadRAM)
		// BBR Layer configuration message
		// NMEA 4.0, NMEA extended svnumbering, dynamic model 7, AssistNow Autonomous, GPS+GAL+BDS+SBAS, 10Hz update rate, disable GLL
		payloadBBR := []byte{0xB5, 0x62, 0x06, 0x8A, 0x28, 0x00, 0x01, 0x02, 0x00, 0x00, 0x01, 0x00, 0x23, 0x10, 0x01, 0x21, 0x00, 0x11, 0x20, 0x07, 0x01, 0x00, 0x21, 0x30, 0x64, 0x00, 0x22, 0x00, 0x31, 0x10, 0x01, 0xCA, 0x00, 0x91, 0x20, 0x00, 0x01, 0x00, 0x93, 0x20, 0x28, 0x07, 0x00, 0x93, 0x20, 0x01, 0x75, 0xF5}
		p.Write(payloadBBR)
	} else if s.GPS_detected_type == common.GPS_TYPE_UBX6 || s.GPS_detected_type == common.GPS_TYPE_UBX7 ||
	          s.GPS_detected_type == common.GPS_TYPE_UBX8 || s.GPS_detected_type == common.GPS_TYPE_UBX9 {

		// Byte order for UBX configuration is little endian.

		// GNSS configuration CFG-GNSS for ublox 7 and higher, p. 125 (v8)

		// Notes: ublox8 is multi-GNSS capable (simultaneous decoding of GPS and GLONASS, or
		// GPS and Galileo) if SBAS (e.g. WAAS) is unavailable. This may provide robustness
		// against jamming / interference on one set of frequencies. However, this will drop the
		// position reporting rate to 5 Hz during times multi-GNSS is in use. This shouldn't affect
		// gpsattitude too much --  without WAAS corrections, the algorithm could get jumpy at higher
		// sampling rates.

		// load default configuration             |      clearMask     |  |     saveMask       |  |     loadMask       |  deviceMask
		p.Write(makeUBXCFG(0x06, 0x09, 13, []byte{0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00, 0x03}))
		time.Sleep(100* time.Millisecond)

		if s.GPS_detected_type == common.GPS_TYPE_UBX9 {
			if globalSettings.DEBUG {
				log.Printf("ublox 9 detected\n")
			}
			// ublox 9
			writeUblox9ConfigCommands(p)		
		} else if s.GPS_detected_type == common.GPS_TYPE_UBX8 {
			if globalSettings.DEBUG {
				log.Printf("ublox 8 detected\n")
			}
			// ublox 8
			writeUblox8ConfigCommands(p)
		} else if (s.GPS_detected_type == common.GPS_TYPE_UBX7) || (s.GPS_detected_type == common.GPS_TYPE_UBX6) {
			if globalSettings.DEBUG {
				log.Printf("ublox 6 or 7 detected\n")
			}
			// ublox 6,7
			cfgGnss := []byte{0x00, 0x00, 0xFF, 0x04} // numTrkChUse=0xFF: number of tracking channels to use will be set to number of tracking channels available in hardware
			gps     := []byte{0x00, 0x04, 0xFF, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GPS with 4-255 channels (ublox default)
			sbas    := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable SBAS with 1-3 channels (ublox default)
			qzss    := []byte{0x05, 0x00, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable QZSS with 0-3 channel (ublox default)
			glonass := []byte{0x06, 0x08, 0xFF, 0x00, 0x00, 0x00, 0x01, 0x01} // disable GLONASS (ublox default)
			cfgGnss = append(cfgGnss, gps...)
			cfgGnss = append(cfgGnss, sbas...)
			cfgGnss = append(cfgGnss, qzss...)
			cfgGnss = append(cfgGnss, glonass...)
			p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss))
		}

		writeUbloxGenericCommands(10, p)

		// Reconfigure serial port.
		cfg := make([]byte, 20)
		cfg[0] = 0x01 // portID.
		cfg[1] = 0x00 // res0.
		cfg[2] = 0x00 // res1.
		cfg[3] = 0x00 // res1.

			
		//      [   7   ] [   6   ] [   5   ] [   4   ]
		//	0000 0000 0000 0000 0000 10x0 1100 0000
		// UART mode. 0 stop bits, no parity, 8 data bits. Little endian order.
		cfg[4] = 0xC0
		cfg[5] = 0x08
		cfg[6] = 0x00
		cfg[7] = 0x00

		// Baud rate. Little endian order.
		bdrt := uint32(115200)
		cfg[11] = byte((bdrt >> 24) & 0xFF)
		cfg[10] = byte((bdrt >> 16) & 0xFF)
		cfg[9] = byte((bdrt >> 8) & 0xFF)
		cfg[8] = byte(bdrt & 0xFF)

		// inProtoMask. NMEA and UBX. Little endian.
		cfg[12] = 0x03
		cfg[13] = 0x00

		// outProtoMask. NMEA. Little endian.
		cfg[14] = 0x02
		cfg[15] = 0x00

		cfg[16] = 0x00 // flags.
		cfg[17] = 0x00 // flags.

		cfg[18] = 0x00 //pad.
		cfg[19] = 0x00 //pad.

		// UBX-CFG-PRT (Port Configuration for UART)
		p.Write(makeUBXCFG(0x06, 0x00, 20, cfg))


		//	time.Sleep(100* time.Millisecond) // pause and wait for the GPS to finish configuring itself before closing / reopening the port
		baudrates[0] = int(bdrt)

		if globalSettings.DEBUG {
			log.Printf("Finished writing u-blox GPS config to %s. Opening port to test connection.\n", device)
		}
	} else if s.GPS_detected_type == common.GPS_TYPE_SOFTRF_DONGLE {
		p.Write([]byte("@GNS 0x7\r\n")) // enable SBAS
		p.Flush()
		time.Sleep(250* time.Millisecond) // Otherwise second command doesn't seem to work?
		p.Write([]byte("@BSSL 0x2D\r\n")) // enable GNGSV
		p.Flush()
	}
	p.Close()

	time.Sleep(250 * time.Millisecond)

	// Re-open port at newly configured baud so we can read messages. ReadTimeout is set to keep from blocking the gpsSerialReader() on misconfigures or ttyAMA disconnects
	// serialConfig = &serial.Config{Name: device, Baud: baudrate, ReadTimeout: time.Millisecond * 2500}
	// serial.OpenPort(serialConfig)
	p, err = s.detectOpenSerialPort(device, baudrates)
	if err != nil {
		log.Printf("serial port err: %s\n", err.Error())
		return false
	}

	s.serialPort = p
	return true
}

func (s serialGPSDevice) detectOpenSerialPort(device string, baudrates []int) (*(serial.Port), error) {
	if len(baudrates) == 1 {
		serialConfig := &serial.Config{Name: device, Baud: baudrates[0], ReadTimeout: time.Millisecond * 2500}
		return serial.OpenPort(serialConfig)
	} else {
		for _, baud := range baudrates {
			serialConfig := &serial.Config{Name: device, Baud: baud, ReadTimeout: time.Millisecond * 2500}
			p, err := serial.OpenPort(serialConfig)
			if err != nil {
				return p, err
			}
			// Check if we get any data...
			time.Sleep(3 * time.Second)
			buffer := make([]byte, 10000)
			p.Read(buffer)
			splitted := strings.Split(string(buffer), "\n")
			for _, line := range splitted {
				_, validNMEAcs := nmea.ValidateNMEAChecksum(line)
				if validNMEAcs {
					// looks a lot like NMEA.. use it
					log.Printf("Detected serial port %s with baud %d", device, baud)
					// Make sure the NMEA is immediately parsed once, so updateStatus() doesn't see the GPS as disconnected before
					// first msg arrives
					
					// TODO: RVT 
					//processNMEALine(line)
					return p, nil
				}
			}
			p.Close()
			time.Sleep(250 * time.Millisecond)
		}
		return nil, errors.New("Failed to detect GPS serial baud rate")
	}
}


func writeUblox8ConfigCommands(p *serial.Port) {
	cfgGnss := []byte{0x00, 0x00, 0xFF, 0x05} // numTrkChUse=0xFF: number of tracking channels to use will be set to number of tracking channels available in hardware
	gps     := []byte{0x00, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GPS with 8-16 channels (ublox default)
	sbas    := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable SBAS with 1-3 channels (ublox default)
	galileo := []byte{0x02, 0x08, 0x08, 0x00, 0x01, 0x00, 0x01, 0x01} // enable Galileo with 8-8 channels (ublox default: disabled and 4-8 channels)
	beidou  := []byte{0x03, 0x08, 0x10, 0x00, 0x00, 0x00, 0x01, 0x01} // disable BEIDOU
	qzss    := []byte{0x05, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable QZSS 1-3 channels, L1C/A (ublox default: 0-3 channels)
	glonass := []byte{0x06, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GLONASS with 8-16 channels (ublox default: 8-14 channels)
	
	cfgGnss = append(cfgGnss, gps...)
	cfgGnss = append(cfgGnss, sbas...)
	cfgGnss = append(cfgGnss, beidou...)
	cfgGnss = append(cfgGnss, qzss...)
	cfgGnss = append(cfgGnss, glonass...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss)) // Succeeds on all chips supporting GPS+GLO

	cfgGnss[3] = 0x06
	cfgGnss = append(cfgGnss, galileo...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss)) // Succeeds only on chips that support GPS+GLO+GAL
}

func writeUblox9ConfigCommands(p *serial.Port) {
	cfgGnss := []byte{0x00, 0x00, 0xFF, 0x06} // numTrkChUse=0xFF: number of tracking channels to use will be set to number of tracking channels available in hardware
	gps     := []byte{0x00, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GPS with 8-16 channels (ublox default)
	sbas    := []byte{0x01, 0x03, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable SBAS with 3-3 channels (ublox default)
	galileo := []byte{0x02, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable Galileo with 8-16 channels (ublox default: 8-12 channels)
	beidou  := []byte{0x03, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable BEIDOU with 8-16 channels (ublox default: 2-5 channels)
	qzss    := []byte{0x05, 0x03, 0x04, 0x00, 0x01, 0x00, 0x05, 0x01} // enable QZSS 3-4 channels, L1C/A & L1S (ublox default)
	glonass := []byte{0x06, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GLONASS with 8-16 tracking channels (ublox default: 8-12 channels)
	
	cfgGnss = append(cfgGnss, gps...)
	cfgGnss = append(cfgGnss, sbas...)
	cfgGnss = append(cfgGnss, beidou...)
	cfgGnss = append(cfgGnss, qzss...)
	cfgGnss = append(cfgGnss, glonass...)
	cfgGnss = append(cfgGnss, galileo...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss))
}

func writeUbloxGenericCommands(navrate uint16, p *serial.Port) {

	// UBX-CFG-NMEA (change NMEA protocol version to 4.0 extended)
	p.Write(makeUBXCFG(0x06, 0x17, 20, []byte{0x00, 0x40, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}))

	// UBX-CFG-PMS
	p.Write(makeUBXCFG(0x06, 0x86, 8, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Full Power Mode
	// p.Write(makeUBXCFG(0x06, 0x86, 8, []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Balanced Power Mode

	// UBX-CFG-NAV5                           |mask1...|  dyn
	p.Write(makeUBXCFG(0x06, 0x24, 36, []byte{0x01, 0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Dynamic platform model: airborne with <2g acceleration

	// UBX-CFG-SBAS (disable integrity, enable auto-scan)
	p.Write(makeUBXCFG(0x06, 0x16, 8, []byte{0x01, 0x03, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00}))

	// UBX-CFG-MSG (NMEA Standard Messages)  msg   msg   Ports 1-6 (every 10th message over UART1, every message over USB)
	//                                       Class ID    DDC   UART1 UART2 USB   I2C   Res
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // GGA - Global positioning system fix data
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GLL - Latitude and longitude, with time of position fix and status
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x02, 0x00, 0x05, 0x00, 0x05, 0x00, 0x00})) // GSA - GNSS DOP and Active Satellites
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x03, 0x00, 0x05, 0x00, 0x05, 0x00, 0x00})) // GSV - GNSS Satellites in View
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x04, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // RMC - Recommended Minimum data
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x05, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // VGT - Course over ground and Ground speed
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GRS - GNSS Range Residuals
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GST - GNSS Pseudo Range Error Statistics
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // ZDA - Time and Date<
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x09, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GBS - GNSS Satellite Fault Detection
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0A, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // DTM - Datum Reference
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0D, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GNS - GNSS fix data
	// p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // ???
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // VLW - Dual ground/water distance

	// UBX-CFG-MSG (NMEA PUBX Messages)      msg   msg   Ports 1-6
	//                                       Class ID    DDC   UART1 UART2 USB   I2C   Res
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Ublox - Lat/Long Position Data
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Ublox - Satellite Status
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Ublox - Time of Day and Clock Information



	if navrate == 10 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0x64, 0x00, 0x01, 0x00, 0x01, 0x00})) // 100ms & 1 cycle -> 10Hz (UBX-CFG-RATE payload bytes: little endian!)
	} else if navrate == 5 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xC8, 0x00, 0x01, 0x00, 0x01, 0x00})) // 200ms & 1 cycle -> 5Hz (UBX-CFG-RATE payload bytes: little endian!)
	} else if navrate == 2 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xF4, 0x01, 0x01, 0x00, 0x01, 0x00})) // 500ms & 1 cycle -> 2Hz (UBX-CFG-RATE payload bytes: little endian!)
	} else if navrate == 1 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xE8, 0x03, 0x01, 0x00, 0x01, 0x00})) // 1000ms & 1 cycle -> 1Hz (UBX-CFG-RATE payload bytes: little endian!)
	}


}


func (s serialGPSDevice) configureOgnTracker() {
	if s.serialPort == nil {
		return
	}

	s.gpsTimeOffsetPpsMs = 200 * time.Millisecond
	s.serialPort.Write([]byte(nmea.AppendNmeaChecksum("$POGNS,NavRate=5") + "\r\n")) // Also force NavRate directly, just to make sure it's always set

	s.serialPort.Write([]byte(getOgnTrackerConfigQueryString())) // query current configuration

	// Configuration for OGN Tracker T-Beam is similar to normal Ublox config
	writeUblox8ConfigCommands(s.serialPort)
	writeUbloxGenericCommands(5, s.serialPort)

	s.serialPort.Flush()

	s.GPS_detected_type = common.GPS_TYPE_OGNTRACKER
}


func (s serialGPSDevice) getOgnTrackerConfigString() string {
	/* TODO: RVT
	msg := fmt.Sprintf("$POGNS,Address=0x%s,AddrType=%d,AcftType=%d,Pilot=%s,Reg=%s,TxPower=%d,Hard=STX,Soft=%s",
		globalSettings.OGNAddr, globalSettings.OGNAddrType, globalSettings.OGNAcftType, globalSettings.OGNPilot, globalSettings.OGNReg, globalSettings.OGNTxPower, stratuxVersion[1:])
	msg = nmea.AppendNmeaChecksum(msg)
	return msg + "\r\n"
	RVT: TODO */
	return ""
}

func getOgnTrackerConfigQueryString() string {
	return nmea.AppendNmeaChecksum("$POGNS") + "\r\n"
}

func (s serialGPSDevice) configureOgnTrackerFromSettings() {
	if s.serialPort == nil {
		return
	}

	cfg := s.getOgnTrackerConfigString()
	log.Printf("Configuring OGN Tracker: %s ", cfg)

	// RVT: TODO s.serialPort.Write([]byte(cfg))
	s.serialPort.Write([]byte(getOgnTrackerConfigQueryString())) // re-read settings from tracker
	s.serialPort.Flush()
}


func (s serialGPSDevice) gpsSerialReader() {
	defer s.serialPort.Close()
	s.readyToInitGPS = false //TODO: replace with channel control to terminate goroutine when complete

	i := 0 //debug monitor
	scanner := bufio.NewScanner(s.serialPort)
	for scanner.Scan() && s.GPS_connected /* && globalSettings.GPS_Enabled */ {
		i++
		if globalSettings.DEBUG && i%100 == 0 {
			log.Printf("gpsSerialReader() scanner loop iteration i=%d\n", i) // debug monitor
		}

		s := scanner.Text()
		startIdx := strings.Index(s, "$")
		if startIdx < 0 {
			continue
		}
		s = s[startIdx:]

		// TODO: RVT HERE IS WHERE THE NMEA STRINGS COME IN
		// if !processNMEALine(s) {
		// 	if globalSettings.DEBUG {
		// 		fmt.Printf("processNMEALine() exited early -- %s\n", s)
		// 	}
		// }
	}
	if err := scanner.Err(); err != nil {
		log.Printf("reading standard input: %s\n", err.Error())
	}

	if globalSettings.DEBUG {
		log.Printf("Exiting gpsSerialReader() after i=%d loops\n", i) // debug monitor
	}
	s.GPS_connected = false
	s.readyToInitGPS = true //TODO: replace with channel control to terminate goroutine when complete
	return
}

func  (s serialGPSDevice) pollGPS() {
	s.readyToInitGPS = true //TODO: Implement more robust method (channel control) to kill zombie serial readers
	timer := time.NewTicker(4 * time.Second)
	for {
		<-timer.C
		// GPS enabled, was not connected previously?
		if !s.GPS_connected && s.readyToInitGPS { //TODO: Implement more robust method (channel control) to kill zombie serial readers
			s.GPS_connected = s.initGPSSerial()
			if s.GPS_connected && (s.GPS_detected_type & 0x0f) != common.GPS_TYPE_NETWORK {
				go s.gpsSerialReader()
			}
		}
	}
}

func (s serialGPSDevice) Listen() {
	go s.pollGPS()
}

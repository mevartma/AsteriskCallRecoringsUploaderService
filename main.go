package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	_ "github.com/aws/aws-sdk-go/aws/awsutil"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	_ "github.com/go-sql-driver/mysql"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	dbURL  string = "root:@tcp(localhost:3306)/asteriskcdrdb"
	server string = "mysql"
)

var (
	bucket   = aws.String("betamediarecording")
	s3Client *s3.S3
)

var s3recordings []RecordingDetails
var setting ServerConfig

type RecordingDetails struct {
	Recording_File string
	CallDate       string
	Disk_File_Path string
	Office         string
}

type ServerConfig struct {
	Office     string `json:"office"`
	Server_URL string `json:"server_url"`
	AWS_ID     string `json:"aws_id"`
	AWS_Key    string `json:"aws_key"`
}

func init() {
	fmt.Println("init")
	file, err := ioutil.ReadFile("config.json")
	if err != nil {
		log.Fatal("Config File Missing. ", err)
		os.Exit(1)
	}

	err = json.Unmarshal(file, &setting)
	if err != nil {
		log.Fatal("Config Parse Error: ", err)
		os.Exit(1)
	}

	s3Config := &aws.Config{
		Credentials:      credentials.NewStaticCredentials(setting.AWS_ID, setting.AWS_Key, ""),
		Region:           aws.String("eu-central-1"),
		DisableSSL:       aws.Bool(true),
		S3ForcePathStyle: aws.Bool(true),
		Endpoint:         aws.String("s3.eu-central-1.amazonaws.com"),
	}

	s3Session, err := session.NewSession(s3Config)
	if err != nil {
		log.Fatal(err)
	}

	s3Client = s3.New(s3Session)
}

func GetRecordingByDate(date string) (*[]RecordingDetails, error) {
	fmt.Println("GetAllRecording")
	var results []RecordingDetails
	db, err := sql.Open(server, dbURL)
	if err != nil {
		return &results, err
	}
	defer db.Close()

	query := "SELECT calldate,recordingfile FROM cdr WHERE calldate like ?"
	rows, err := db.Query(query, date)
	if err != nil {
		return &results, err
	}

	for rows.Next() {
		var r RecordingDetails
		err = rows.Scan(&r.CallDate, &r.Recording_File)
		if err != nil {
			return &results, err
		}
		results = append(results, r)
	}

	return &results, err
}

func GetAllRecording() (*[]RecordingDetails, error) {
	fmt.Println("GetAllRecording")
	var results []RecordingDetails
	db, err := sql.Open(server, dbURL)
	if err != nil {
		return &results, err
	}
	defer db.Close()

	query := "SELECT recordingfile FROM cdr"
	rows, err := db.Query(query)
	if err != nil {
		return &results, err
	}

	for rows.Next() {
		var r RecordingDetails
		err = rows.Scan(&r.CallDate, &r.Recording_File)
		if err != nil {
			return &results, err
		}
		results = append(results, r)
	}

	return &results, err
}

func updateRecords(c, d string) error {
	switch c {
	case "all":
		rss, err := GetAllRecording()
		if err != nil {
			return err
		}
		s3recordings = nil
		for _, rs := range *rss {
			s3recordings = append(s3recordings, rs)
		}
		return err
	case "date":
		now := time.Now()
		newDate := now.Format("2006-01-02")
		newDate += "%"
		rss, err := GetRecordingByDate(newDate)
		if err != nil {
			return err
		}
		for _, rs := range *rss {
			s3recordings = append(s3recordings, rs)
		}
		return err
	case "from":
		newDate := d
		newDate += "%"
		rss, err := GetRecordingByDate(newDate)
		if err != nil {
			return err
		}
		for _, rs := range *rss {
			s3recordings = append(s3recordings, rs)
		}
		return err
	default:
		err := errors.New("No Action found")
		return err
	}
	return nil
}

func findRecord(recordDate, recordName, officeName string) string {
	var basePath string
	if strings.ToLower(officeName) == "germany" {
		basePath = "/var/spool/asterisk/monitor"
	} else if strings.ToLower(officeName) == "kiev" {
		basePath = "/home/recording"
	}

	s := strings.Split(recordDate, " ")
	date := strings.Split(s[0], "-")
	g := fmt.Sprintf("%s/%s/%s/%s/%s", basePath, date[0], date[1], date[2], recordName)

	return g
}

func main() {
	fmt.Println("main")
	command := flag.String("c", "all", "all data or by day")
	date := flag.String("d", "2006-01-02", "from date until now")
	flag.Parse()
	err := updateRecords(*command, *date)
	if err != nil {
		log.Fatal("Faild to get data from database", err)
		os.Exit(1)
	}

	for _, record := range s3recordings {
		if record.Recording_File != "" {
			DiskFilePath := findRecord(record.CallDate, record.Recording_File, setting.Office)
			record.Disk_File_Path = DiskFilePath
			record.Office = setting.Office

			if _, err := os.Stat(record.Disk_File_Path); err == nil {
				file, err := os.Open(record.Disk_File_Path)
				if err != nil {
					log.Println(err)
				}

				fileInfo, _ := file.Stat()
				var size int64 = fileInfo.Size()
				buffer := make([]byte, size)
				file.Read(buffer)
				fileBytes := bytes.NewReader(buffer)
				fileType := http.DetectContentType(buffer)
				filePath := fmt.Sprintf("/%s/%s", record.Office, record.Recording_File)
				file.Close()

				params := &s3.PutObjectInput{
					Bucket:        bucket,
					Key:           aws.String(filePath),
					ACL:           aws.String("public-read"),
					Body:          fileBytes,
					ContentLength: aws.Int64(size),
					ContentType:   aws.String(fileType),
					Metadata: map[string]*string{
						"key": aws.String("MetadataValue"),
					},
				}

				_, err = s3Client.PutObject(params)
				if err != nil {
					log.Println(err)
				}

				fileURL := fmt.Sprintf("https://s3.eu-central-1.amazonaws.com/betamediarecording/%s/%s", record.Office, record.Recording_File)
				fmt.Printf("%s\r\n", fileURL)
			}
		}
	}

	os.Exit(1)
}

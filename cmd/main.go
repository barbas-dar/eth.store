package main

import (
	"bytes"
	"cloud.google.com/go/bigquery"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	ethstore "github.com/gobitfly/eth.store"
	"github.com/gobitfly/eth.store/version"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"
)

var opts struct {
	Days         string
	Validators   string
	ConsAddress  string
	ConsTimeout  time.Duration
	ExecAddress  string
	ExecTimeout  time.Duration
	Json         bool
	JsonFile     string
	DebugLevel   uint64
	Version      bool
	ReceiptsMode int
	Upload       bool
	Final        bool
}

func main() {
	flag.StringVar(&opts.Days, "days", "", "days to calculate eth.store for, format: \"1-3\" or \"1,4,6\"")
	flag.StringVar(&opts.ConsAddress, "cons.address", "http://localhost:4000", "address of the conensus-node-api")
	flag.DurationVar(&opts.ConsTimeout, "cons.timeout", time.Second*120, "timeout duration for the consensus-node-api")
	flag.StringVar(&opts.ExecAddress, "exec.address", "http://localhost:4000", "address of the execution-node-api")
	flag.DurationVar(&opts.ExecTimeout, "exec.timeout", time.Second*120, "timeout duration for the execution-node-api")
	flag.BoolVar(&opts.Json, "json", false, "format output as json")
	flag.StringVar(&opts.JsonFile, "json.file", "", "path to file to write results into, only missing days will be added")
	flag.Uint64Var(&opts.DebugLevel, "debug", 0, "set debug-level (higher level will increase verbosity)")
	flag.BoolVar(&opts.Version, "version", false, "print version and exit")
	flag.IntVar(&opts.ReceiptsMode, "receipts-mode", 0, "mode to use for fetching tx receipts, 0 = eth_getTransactionReceipt, 1 = eth_getBlockReceipts")
	flag.BoolVar(&opts.Upload, "upload", false, "upload json to Big Query and write CSV to GCS")
	flag.BoolVar(&opts.Final, "final.day", false, "final day")
	flag.Parse()

	if opts.Upload && !opts.Json {
		log.Fatalf("invalid flag combination, upload flag must be accompanied by a json flag")
	}

	if opts.Version {
		fmt.Println(version.Version)
		return
	}

	if opts.ReceiptsMode != 0 && opts.ReceiptsMode != 1 {
		log.Fatalf("invalid receipts mode provided, can only be 0 or 1")
	}

	ethstore.SetConsTimeout(opts.ConsTimeout)
	ethstore.SetExecTimeout(opts.ExecTimeout)
	ethstore.SetDebugLevel(opts.DebugLevel)

	days := []uint64{}

	if opts.Days == "all" {
		opts.Days = "0-finalized"
	}

	if strings.ContainsAny(opts.Days, "-") {
		daysSplit := strings.Split(opts.Days, "-")
		fromDay, err := strconv.ParseUint(daysSplit[0], 10, 64)
		if err != nil {
			log.Fatalf("error parsing days-flag: %v", err)
		}
		var toDay uint64
		if daysSplit[1] == "finalized" {
			d, err := ethstore.GetFinalizedDay(context.Background(), opts.ConsAddress)
			if err != nil {
				log.Fatalf("error getting latest day: %v", err)
			}
			toDay = d
		} else if daysSplit[1] == "head" {
			d, err := ethstore.GetHeadDay(context.Background(), opts.ConsAddress)
			if err != nil {
				log.Fatalf("error getting latest day: %v", err)
			}
			toDay = d
		} else {
			d, err := strconv.ParseUint(daysSplit[1], 10, 64)
			if err != nil {
				log.Fatalf("error parsing days-flag: %v", err)
			}
			toDay = d
		}
		if toDay < fromDay {
			log.Fatalf("error parsing days-flag: toDay < fromDay")
		}
		for i := fromDay; i <= toDay; i++ {
			days = append(days, i)
		}
	} else if strings.ContainsAny(opts.Days, ",") {
		s := strings.Split(opts.Days, ",")
		for _, d := range s {
			di, err := strconv.ParseUint(d, 10, 64)
			if err != nil {
				log.Fatalf("error parsing days-flag: %v", err)
			}
			days = append(days, di)
		}
	} else if opts.Days == "finalized" {
		d, err := ethstore.GetFinalizedDay(context.Background(), opts.ConsAddress)
		if err != nil {
			log.Fatalf("error getting lattest day: %v", err)
		}
		days = []uint64{d}
	} else if opts.Days == "head" {
		d, err := ethstore.GetHeadDay(context.Background(), opts.ConsAddress)
		if err != nil {
			log.Fatalf("error getting lattest day: %v", err)
		}
		days = []uint64{d}
	} else {
		d, err := strconv.ParseUint(opts.Days, 10, 64)
		if err != nil {
			log.Fatalf("error parsing days-flag: %v", err)
		}
		days = []uint64{d}
	}

	if opts.JsonFile != "" && opts.Days != "head" {
		fileDays := []*ethstore.Day{}
		_, err := os.Stat(opts.JsonFile)
		if err == nil {
			fileDaysBytes, err := os.ReadFile(opts.JsonFile)
			if err != nil {
				log.Fatalf("error reading file: %v", err)
			}
			err = json.Unmarshal(fileDaysBytes, &fileDays)
			if err != nil {
				log.Fatalf("error parsing file: %v", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Fatalf("error reading file: %v", err)
		}

		fileDaysMap := map[uint64]*ethstore.Day{}
		for _, d := range fileDays {
			fileDaysMap[d.Day.BigInt().Uint64()] = d
		}
		for _, dd := range days {
			if d, exists := fileDaysMap[dd]; exists {
				logEthstoreDay(d)
				continue
			}
			d, _, err := ethstore.Calculate(context.Background(), opts.ConsAddress, opts.ExecAddress, fmt.Sprintf("%d", dd), 10, opts.ReceiptsMode)
			if err != nil {
				log.Fatalf("error calculating ethstore: %v", err)
			}
			fileDays = append(fileDays, d)
			sort.SliceStable(fileDays, func(i, j int) bool {
				return fileDays[i].Day.Cmp(fileDays[j].Day) < 1
			})
			fileDaysJson, err := json.MarshalIndent(&fileDays, "", "\t")
			if err != nil {
				log.Fatalf("error marshaling ethstore: %v", err)
			}
			err = os.WriteFile(opts.JsonFile, fileDaysJson, 0644)
			if err != nil {
				log.Fatalf("error writing ethstore to file: %v", err)
			}
			if !opts.Json {
				logEthstoreDay(d)
			}
		}
	} else {
		result := []*ethstore.Day{}
		if opts.Final {
			d, _, err := ethstore.Calculate(context.Background(), opts.ConsAddress, opts.ExecAddress, "head", 10, opts.ReceiptsMode)
			if err != nil {
				log.Fatalf("error calculating ethstore: %v", err)
			}
			result = append(result, d)
			if !opts.Json {
				logEthstoreDay(d)
			}
		} else {
			for _, dd := range days {
				d, _, err := ethstore.Calculate(context.Background(), opts.ConsAddress, opts.ExecAddress, fmt.Sprintf("%d", dd), 10, opts.ReceiptsMode)
				if err != nil {
					log.Fatalf("error calculating ethstore: %v", err)
				}
				result = append(result, d)
				if !opts.Json {
					logEthstoreDay(d)
				}
			}
		}
		if opts.Json {
			// Turn into a new line delimitted JSON
			var daysJson []byte
			for _, item := range result {
				jsonItem, err := json.Marshal(item)
				if err != nil {
					log.Fatalf("error marshaling ethstore: %v", err)
				}
				daysJson = append(daysJson, jsonItem...)

				// Creates new line delimited JSON by '\n'
				daysJson = append(daysJson, '\n')
			}
			bqFormattedJsonString := string(daysJson)

			if opts.Upload {
				upload(string(bqFormattedJsonString))
			}

			// Original Printing Code here:
			daysJsonWithMarshalIndent, err := json.MarshalIndent(&result, "", "\t")
			if err != nil {
				log.Fatalf("error marshaling ethstore: %v", err)
			}
			fmt.Printf("%s\n", daysJsonWithMarshalIndent)
		}
	}
}

func logEthstoreDay(d *ethstore.Day) {
	fmt.Printf("day: %v (%v), epochs: %v-%v, validators: %v, apr: %v, effectiveBalanceSumGwei: %v, totalRewardsSumWei: %v, consensusRewardsGwei: %v (%s%%), txFeesSumWei: %v\n", d.Day, d.DayTime, d.StartEpoch, d.StartEpoch.Add(decimal.New(224, 0)), d.Validators, d.Apr.StringFixed(9), d.EffectiveBalanceGwei, d.TotalRewardsWei, d.ConsensusRewardsGwei, d.ConsensusRewardsGwei.Mul(decimal.NewFromInt(1e9*1e2)).Div(d.TotalRewardsWei).StringFixed(2), d.TxFeesSumWei)
}

func upload(jsonData string) {
	if err := godotenv.Load(); err != nil {
		fmt.Println("Error loading .env file")
	}

	// Print new line delimited json in case of error
	fmt.Println(jsonData)

	projectID := os.Getenv("projectID")
	datasetID := os.Getenv("datasetID")
	tableID := os.Getenv("tableID")

	ctx := context.Background()
	client, err := bigquery.NewClient(ctx, projectID)

	if err != nil {
		log.Fatalf("Failed to create BigQuery client: %v", err)
	}

	defer client.Close()

	reader := strings.NewReader(jsonData)
	source := bigquery.NewReaderSource(reader)
	source.SourceFormat = bigquery.JSON

	source.Schema = bigquery.Schema{
		{Name: "day", Type: bigquery.IntegerFieldType},
		{Name: "dayTime", Type: bigquery.TimestampFieldType},
		{Name: "apr", Type: bigquery.FloatFieldType},
		{Name: "validators", Type: bigquery.IntegerFieldType},
		{Name: "startEpoch", Type: bigquery.IntegerFieldType},
		{Name: "effectiveBalanceGwei", Type: bigquery.BigNumericFieldType},
		{Name: "startBalanceGwei", Type: bigquery.BigNumericFieldType},
		{Name: "endBalanceGwei", Type: bigquery.BigNumericFieldType},
		{Name: "depositsSumGwei", Type: bigquery.BigNumericFieldType},
		{Name: "withdrawalsSumGwei", Type: bigquery.BigNumericFieldType},
		{Name: "consensusRewardsGwei", Type: bigquery.BigNumericFieldType},
		{Name: "txFeesSumWei", Type: bigquery.BigNumericFieldType},
		{Name: "totalRewardsWei", Type: bigquery.BigNumericFieldType},
	}

	fmt.Println(source)
	loader := client.Dataset(datasetID).Table(tableID).LoaderFrom(source)

	job, err := loader.Run(ctx)

	if err != nil {
		log.Fatalf("Failed to run load job: %v", err)
	}

	status, err := job.Wait(ctx)

	if err != nil {
		log.Fatalf("Failed to wait for job: %v", err)
	}

	if err := status.Err(); err != nil {
		log.Fatalf("Job failed: %v", err)
	}

	fmt.Println("Upload to GCP completed successfully")
	fmt.Println("Writing to CSV...")
	csv := new(bytes.Buffer)

	err = query(csv, projectID)
	if err != nil {
		return
	}

	fmt.Println("Write to CSV completed successfully")
}

func query(w io.Writer, projectID string) error {
	ctx := context.Background()
	client, err := bigquery.NewClient(ctx, projectID)
	if err != nil {
		return fmt.Errorf("bigquery.NewClient: %v", err)
	}
	defer client.Close()

	q := client.Query(
		"select * from (select" +
			"\"Ethereum\" AS blockchain," +
			"\"DASK8KY\" AS darAssetID," +
			"\"ETH\" AS darAssetTicker," +
			"\"BMFHC58\" AS sedol," +
			"\"daily\" AS periodType," +
			"FORMAT_TIMESTAMP('%Y-%m-%d %H:%M:00', dayTime) AS rewardPeriodStartTime," +
			"FORMAT_TIMESTAMP('%Y-%m-%d %H:%M:00', DATE_ADD(dayTime, INTERVAL 1 DAY)) AS rewardPeriodEndTime," +
			"FORMAT('%0.6f', consensusRewardsGwei/1000000000.0) AS consensusRewardQuantity," +
			"FORMAT('%0.6f', (totalRewardsWei - (consensusRewardsGwei*1000000000.0))/1000000000000000000.0) as transactionRewardQuantity," +
			"FORMAT('%0.6f', totalRewardsWei/1000000000000000000.0) AS rewardQuantity," +
			"FORMAT('%0.6f', effectiveBalanceGwei/1000000000.0) AS stakedQuantity," +
			"\"\" AS reserved1," +
			"\"\" AS reserved2," +
			"\"\" AS reserved3," +
			"\"\" AS reserved4," +
			"\"\" AS reserved5," +
			"\"\" AS reserved6," +
			"\"\" AS reserved7," +
			"\"\" AS reserved8," +
			"\"\" AS reserved9," +
			"\"\" AS reserved10," +
			"FROM `dar-data-lake.blockchain_ethereum2.daily_rewards`" +
			"ORDER BY day desc\n" +
			"limit 30) order by rewardPeriodStartTime asc")

	// Location must match that of the dataset(s) referenced in the query.
	q.Location = "US"
	// Run the query and print results when the query job is completed.
	job, err := q.Run(ctx)
	if err != nil {
		return err
	}
	status, err := job.Wait(ctx)
	if err != nil {
		return err
	}
	if err := status.Err(); err != nil {
		return err
	}

	jc, err := job.Config()
	if err != nil {
		// TODO: Handle error.
	}

	jobConfig := jc.(*bigquery.QueryConfig)

	destProjectId := jobConfig.Dst.ProjectID
	destTableId := jobConfig.Dst.TableID
	destDataSetId := jobConfig.Dst.DatasetID

	now := time.Now().Format("20060102")

	gcsUri := fmt.Sprintf("gs://dar-dev-02-eth-data/%s_DAR_ETH.csv", now)

	err = export(destTableId, destDataSetId, destProjectId, gcsUri)
	if err != nil {
		return err
	}

	return nil
}

func export(tableID string, dataSetID string, sourceProjectID string, gcsURI string) error {
	ctx := context.Background()
	client, err := bigquery.NewClient(ctx, sourceProjectID)
	if err != nil {
		return fmt.Errorf("bigquery.NewClient: %v", err)
	}
	defer client.Close()

	gcsRef := bigquery.NewGCSReference(gcsURI)
	gcsRef.FieldDelimiter = ","

	extractor := client.DatasetInProject(sourceProjectID, dataSetID).Table(tableID).ExtractorTo(gcsRef)
	extractor.DisableHeader = false
	// You can choose to run the job in a specific location for more complex data locality scenarios.
	// Ex: In this example, source dataset and GCS bucket are in the US.
	extractor.Location = "US"

	job, err := extractor.Run(ctx)
	if err != nil {
		return err
	}
	status, err := job.Wait(ctx)
	if err != nil {
		return err
	}
	if err := status.Err(); err != nil {
		return err
	}
	return nil
}

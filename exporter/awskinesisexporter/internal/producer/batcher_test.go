// Copyright  OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package producer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/aws/aws-sdk-go/service/kinesis/kinesisiface"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.uber.org/zap/zaptest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awskinesisexporter/internal/batch"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awskinesisexporter/internal/producer"
)

type MockKinesisAPI struct {
	kinesisiface.KinesisAPI

	op func(*kinesis.PutRecordsInput) (*kinesis.PutRecordsOutput, error)
}

var _ kinesisiface.KinesisAPI = (*MockKinesisAPI)(nil)

func (mka *MockKinesisAPI) PutRecordsWithContext(ctx context.Context, r *kinesis.PutRecordsInput, opts ...request.Option) (*kinesis.PutRecordsOutput, error) {
	return mka.op(r)
}

func SetPutRecordsOperation(op func(r *kinesis.PutRecordsInput) (*kinesis.PutRecordsOutput, error)) kinesisiface.KinesisAPI {
	return &MockKinesisAPI{op: op}
}

func SuccessfulPutRecordsOperation(_ *kinesis.PutRecordsInput) (*kinesis.PutRecordsOutput, error) {
	return &kinesis.PutRecordsOutput{
		FailedRecordCount: aws.Int64(0),
		Records: []*kinesis.PutRecordsResultEntry{
			{ShardId: aws.String("0000000000000000000001"), SequenceNumber: aws.String("0000000000000000000001")},
		},
	}, nil
}

func HardFailedPutRecordsOperation(r *kinesis.PutRecordsInput) (*kinesis.PutRecordsOutput, error) {
	return &kinesis.PutRecordsOutput{FailedRecordCount: aws.Int64(int64(len(r.Records)))}, awserr.New(
		kinesis.ErrCodeResourceNotFoundException,
		"testing incorrect kinesis configuration",
		errors.New("test case failure"),
	)
}

func TransiantPutRecordsOperation(recoverAfter int) func(_ *kinesis.PutRecordsInput) (*kinesis.PutRecordsOutput, error) {
	attempt := 0
	return func(r *kinesis.PutRecordsInput) (*kinesis.PutRecordsOutput, error) {
		if attempt < recoverAfter {
			attempt++
			return &kinesis.PutRecordsOutput{FailedRecordCount: aws.Int64(int64(len(r.Records)))}, awserr.New(
				kinesis.ErrCodeProvisionedThroughputExceededException,
				"testing throttled kinesis operation",
				errors.New("test case throttled"),
			)
		}
		return SuccessfulPutRecordsOperation(r)
	}
}

func TestBatchedExporter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		PutRecordsOP func(*kinesis.PutRecordsInput) (*kinesis.PutRecordsOutput, error)
		shouldErr    bool
		isPermanent  bool
	}{
		{name: "Successful put to kinesis", PutRecordsOP: SuccessfulPutRecordsOperation, shouldErr: false, isPermanent: false},
		{name: "Invalid kinesis configuration", PutRecordsOP: HardFailedPutRecordsOperation, shouldErr: true, isPermanent: true},
		{name: "Test throttled kinesis operation", PutRecordsOP: TransiantPutRecordsOperation(2), shouldErr: true, isPermanent: false},
	}

	bt := batch.New()
	for i := 0; i < 500; i++ {
		assert.NoError(t, bt.AddProtobufV1(new(empty.Empty), "fixed-key"))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			be, err := producer.NewBatcher(
				SetPutRecordsOperation(tc.PutRecordsOP),
				tc.name,
				producer.WithLogger(zaptest.NewLogger(t)),
			)
			require.NoError(t, err, "Must not error when creating BatchedExporter")
			require.NotNil(t, be, "Must have a valid client to use")

			err = be.Put(context.Background(), bt)
			if !tc.shouldErr {
				assert.NoError(t, err, "Must not have returned an error for this test case")
				return
			}

			assert.Error(t, err, "Must have returned an error for this test case")
			if tc.isPermanent {
				assert.True(t, consumererror.IsPermanent(err), "Must have returned a permanent error")
			}
		})
	}
}

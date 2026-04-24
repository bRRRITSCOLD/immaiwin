package polymarket

import (
	"fmt"
	"testing"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/clob/ws"
	"github.com/stretchr/testify/suite"
)

type DetectorTestSuite struct {
	suite.Suite
}

func TestDetectorTestSuite(t *testing.T) {
	suite.Run(t, new(DetectorTestSuite))
}

func (s *DetectorTestSuite) SetupSuite() {}

func (s *DetectorTestSuite) TearDownSuite() {}

func (s *DetectorTestSuite) SetupTest() {}

func (s *DetectorTestSuite) TearDownTest() {}

func (s *DetectorTestSuite) makeEvent(assetID, size string) ws.LastTradePriceEvent {
	return ws.LastTradePriceEvent{
		AssetID: assetID,
		Market:  "0xmarket",
		Price:   "0.55",
		Side:    "BUY",
		Size:    size,
	}
}

func (s *DetectorTestSuite) TestBelowBothThresholds_NotFlagged() {
	det := NewDetector(DetectorConfig{WindowSize: 5, SizeMultiplier: 3.0, MinAbsoluteSize: 10000})

	result, detected := det.Process(s.makeEvent("asset-1", "100"))

	s.False(detected)
	s.Nil(result)
}

func (s *DetectorTestSuite) TestAbsoluteThreshold_Flagged() {
	det := NewDetector(DetectorConfig{WindowSize: 5, SizeMultiplier: 3.0, MinAbsoluteSize: 10000})

	result, detected := det.Process(s.makeEvent("asset-1", "15000"))

	s.True(detected)
	s.Require().NotNil(result)
	s.Equal("asset-1", result.AssetID)
	s.Contains(result.Reason, "absolute threshold")
}

func (s *DetectorTestSuite) TestSizeMultiplier_FlaggedAfterWindowFills() {
	det := NewDetector(DetectorConfig{WindowSize: 3, SizeMultiplier: 3.0, MinAbsoluteSize: 999999})

	// Feed three trades of 100 to fill the window (below absolute threshold).
	for i := range 3 {
		_, detected := det.Process(s.makeEvent("asset-2", fmt.Sprintf("10%d", i)))
		s.False(detected, "setup trades should not be flagged")
	}

	// A trade at 3× the average (~300) should be flagged.
	result, detected := det.Process(s.makeEvent("asset-2", "900"))

	s.True(detected)
	s.Require().NotNil(result)
	s.Contains(result.Reason, "rolling avg")
}

func (s *DetectorTestSuite) TestSizeMultiplier_NotFlaggedBeforeWindowFills() {
	det := NewDetector(DetectorConfig{WindowSize: 5, SizeMultiplier: 3.0, MinAbsoluteSize: 999999})

	// Only 2 trades — window not yet full, multiplier check should not trigger.
	det.Process(s.makeEvent("asset-3", "100"))
	result, detected := det.Process(s.makeEvent("asset-3", "900"))

	s.False(detected)
	s.Nil(result)
}

func (s *DetectorTestSuite) TestUnparsableSize_Ignored() {
	det := NewDetector(DetectorConfig{WindowSize: 5, SizeMultiplier: 3.0, MinAbsoluteSize: 1})

	result, detected := det.Process(s.makeEvent("asset-4", "not-a-number"))

	s.False(detected)
	s.Nil(result)
}

func (s *DetectorTestSuite) TestZeroSize_Ignored() {
	det := NewDetector(DetectorConfig{WindowSize: 5, SizeMultiplier: 3.0, MinAbsoluteSize: 1})

	result, detected := det.Process(s.makeEvent("asset-5", "0"))

	s.False(detected)
	s.Nil(result)
}

func (s *DetectorTestSuite) TestIsolatedPerAsset() {
	det := NewDetector(DetectorConfig{WindowSize: 3, SizeMultiplier: 3.0, MinAbsoluteSize: 999999})

	// Fill window for asset-A.
	for range 3 {
		det.Process(s.makeEvent("asset-A", "100"))
	}

	// asset-B has an empty window — multiplier check should not trigger.
	result, detected := det.Process(s.makeEvent("asset-B", "900"))

	s.False(detected)
	s.Nil(result)
}

// rollingWindow unit tests

type RollingWindowTestSuite struct {
	suite.Suite
}

func TestRollingWindowTestSuite(t *testing.T) {
	suite.Run(t, new(RollingWindowTestSuite))
}

func (s *RollingWindowTestSuite) SetupSuite() {}

func (s *RollingWindowTestSuite) TearDownSuite() {}

func (s *RollingWindowTestSuite) SetupTest() {}

func (s *RollingWindowTestSuite) TearDownTest() {}

func (s *RollingWindowTestSuite) TestAvgBeforeFull_PartialWindow() {
	rw := newRollingWindow(4)
	rw.add(10)
	rw.add(20)

	s.InDelta(15.0, rw.avg(), 0.001)
	s.False(rw.full)
}

func (s *RollingWindowTestSuite) TestAvgAfterFull_AllValues() {
	rw := newRollingWindow(3)
	rw.add(10)
	rw.add(20)
	rw.add(30)

	s.InDelta(20.0, rw.avg(), 0.001)
	s.True(rw.full)
}

func (s *RollingWindowTestSuite) TestAvgAfterWrap_OldestEvicted() {
	rw := newRollingWindow(3)
	rw.add(10)
	rw.add(20)
	rw.add(30)
	rw.add(40) // evicts 10

	// Window now contains 20, 30, 40.
	s.InDelta(30.0, rw.avg(), 0.001)
}

func (s *RollingWindowTestSuite) TestAvgEmpty_ReturnsZero() {
	rw := newRollingWindow(3)

	s.Equal(0.0, rw.avg())
}

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/docker/docker/api/types"
	mock_agentgrpc "github.com/forta-network/forta-node/clients/agentgrpc/mocks"
	mock_clients "github.com/forta-network/forta-node/clients/mocks"
	"github.com/forta-network/forta-node/config"
	mock_containers "github.com/forta-network/forta-node/services/components/containers/mocks"
	mock_lifecycle "github.com/forta-network/forta-node/services/components/lifecycle/mocks"
	mock_metrics "github.com/forta-network/forta-node/services/components/metrics/mocks"
	mock_registry "github.com/forta-network/forta-node/services/components/registry/mocks"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// BotLifecycleManagerTestSuite has unit tests for the bot lifecycle manager.
type BotLifecycleManagerTestSuite struct {
	r *require.Assertions

	msgClient        *mock_clients.MockMessageClient
	lifecycleMetrics *mock_metrics.MockLifecycle
	botGrpc          *mock_agentgrpc.MockClient
	botRegistry      *mock_registry.MockBotRegistry
	botContainers    *mock_containers.MockBotClient
	botPool          *mock_lifecycle.MockBotPoolUpdater
	botMonitor       *mock_lifecycle.MockBotMonitor

	botManager *botLifecycleManager

	suite.Suite
}

func TestBotLifecycleManagerTestSuite(t *testing.T) {
	suite.Run(t, &BotLifecycleManagerTestSuite{})
}

func (s *BotLifecycleManagerTestSuite) SetupTest() {
	s.r = s.Require()
	botRemoveTimeout = 0

	ctrl := gomock.NewController(s.T())
	s.msgClient = mock_clients.NewMockMessageClient(ctrl)
	s.lifecycleMetrics = mock_metrics.NewMockLifecycle(ctrl)
	s.botGrpc = mock_agentgrpc.NewMockClient(ctrl)
	s.botRegistry = mock_registry.NewMockBotRegistry(ctrl)
	s.botContainers = mock_containers.NewMockBotClient(ctrl)
	s.botPool = mock_lifecycle.NewMockBotPoolUpdater(ctrl)
	s.botMonitor = mock_lifecycle.NewMockBotMonitor(ctrl)

	s.botManager = NewManager(s.botRegistry, s.botContainers, s.botPool, s.lifecycleMetrics, s.botMonitor)
}

func (s *BotLifecycleManagerTestSuite) TestAddUpdateRemove() {
	alreadyRunning := []config.AgentConfig{
		{
			ID:    testBotID1,
			Image: testImageRef,
		},
		{
			ID:    testBotID2,
			Image: testImageRef,
		},
	}
	latestAssigned := []config.AgentConfig{
		{
			ID:    testBotID3,
			Image: testImageRef,
		},
		{
			ID:    testBotID1,
			Image: testImageRef,
			ShardConfig: &config.ShardConfig{
				ShardID: 1,
			},
		},
	}
	addedBot := latestAssigned[0]
	removedBot := alreadyRunning[1]

	s.botManager.runningBots = alreadyRunning

	s.botRegistry.EXPECT().LoadAssignedBots().Return(latestAssigned, nil).Times(1)

	s.botContainers.EXPECT().EnsureBotImages(gomock.Any(), []config.AgentConfig{addedBot}).Return([]error{nil}).Times(1)
	s.botContainers.EXPECT().LaunchBot(gomock.Any(), addedBot).Return(nil).Times(1)

	s.botPool.EXPECT().RemoveBotsWithConfigs([]config.AgentConfig{removedBot})
	s.lifecycleMetrics.EXPECT().StatusStopping([]config.AgentConfig{removedBot})
	s.botContainers.EXPECT().TearDownBot(gomock.Any(), removedBot.ContainerName(), true)

	s.lifecycleMetrics.EXPECT().StatusRunning(latestAssigned).Times(1)
	s.botPool.EXPECT().UpdateBotsWithLatestConfigs(latestAssigned)
	s.botMonitor.EXPECT().MonitorBots(GetBotIDs(latestAssigned))

	s.r.NoError(s.botManager.ManageBots(context.Background()))
}

func (s *BotLifecycleManagerTestSuite) TestLoadBotsError() {
	err := errors.New("test err asigned bots")
	s.botRegistry.EXPECT().LoadAssignedBots().Return(nil, err).Times(1)

	s.lifecycleMetrics.EXPECT().SystemError("load.assigned.bots", err)

	s.r.Error(s.botManager.ManageBots(context.Background()))
}

func (s *BotLifecycleManagerTestSuite) TestRestart() {
	botConfigs := []config.AgentConfig{
		{
			ID:    testBotID1,
			Image: testImageRef,
		},
		{
			ID:    testBotID2,
			Image: testImageRef,
		},
	}

	s.botManager.runningBots = botConfigs

	dockerContainerName1 := fmt.Sprintf("/%s", botConfigs[0].ContainerName())
	dockerContainerName2 := fmt.Sprintf("/%s", botConfigs[1].ContainerName())

	s.botContainers.EXPECT().LoadBotContainers(gomock.Any()).Return([]types.Container{
		{
			ID:    testContainerID1,
			Names: []string{dockerContainerName1},
			State: "exited",
		},
		{
			ID:    testContainerID2,
			Names: []string{dockerContainerName2},
			State: "exited",
		},
	}, nil).Times(1)

	s.lifecycleMetrics.EXPECT().ActionRestart(botConfigs[0])
	s.botContainers.EXPECT().StartWaitBotContainer(gomock.Any(), testContainerID1).Return(nil)

	s.lifecycleMetrics.EXPECT().ActionRestart(botConfigs[1])
	err := errors.New("failed to start")
	s.lifecycleMetrics.EXPECT().BotError("start.exited.bot.container", gomock.Any(), testBotID2)
	s.botContainers.EXPECT().StartWaitBotContainer(gomock.Any(), testContainerID2).Return(err)

	// reinitialize only
	s.botPool.EXPECT().ReconnectToBotsWithConfigs([]config.AgentConfig{botConfigs[0]})

	s.r.NoError(s.botManager.RestartExitedBots(context.Background()))
}

func (s *BotLifecycleManagerTestSuite) TestExit() {
	botConfigs := []config.AgentConfig{
		{
			ID:    testBotID1,
			Image: testImageRef,
		},
		{
			ID:    testBotID2,
			Image: testImageRef,
		},
	}

	s.botManager.runningBots = botConfigs

	s.botMonitor.EXPECT().GetInactiveBots().Return([]string{testBotID2})
	s.botContainers.EXPECT().StopBot(gomock.Any(), botConfigs[1])

	s.r.NoError(s.botManager.ExitInactiveBots(context.Background()))
}

func (s *BotLifecycleManagerTestSuite) TestCleanup() {
	botConfigs := []config.AgentConfig{
		{
			ID:    testBotID1,
			Image: testImageRef,
		},
	}

	unusedBotConfig := config.AgentConfig{
		ID:    testBotID2,
		Image: testImageRef,
	}

	s.botManager.runningBots = botConfigs

	dockerContainerName := fmt.Sprintf("/%s", unusedBotConfig.ContainerName())

	s.botContainers.EXPECT().LoadBotContainers(gomock.Any()).Return([]types.Container{
		{
			ID:    testContainerID,
			Names: []string{dockerContainerName},
			State: "exited",
		},
	}, nil).Times(1)
	s.botContainers.EXPECT().TearDownBot(gomock.Any(), unusedBotConfig.ContainerName(), true).Return(nil)

	s.r.NoError(s.botManager.CleanupUnusedBots(context.Background()))
}

func (s *BotLifecycleManagerTestSuite) TestTearDown() {
	botConfigs := []config.AgentConfig{
		{
			ID:    testBotID1,
			Image: testImageRef,
		},
		{
			ID:    testBotID2,
			Image: testImageRef,
		},
	}
	s.botManager.runningBots = botConfigs

	s.botPool.EXPECT().RemoveBotsWithConfigs(botConfigs)
	s.botContainers.EXPECT().TearDownBot(gomock.Any(), botConfigs[0].ContainerName(), false).Return(nil)
	s.botContainers.EXPECT().TearDownBot(gomock.Any(), botConfigs[1].ContainerName(), false).Return(nil)

	s.botManager.TearDownRunningBots(context.Background())
}

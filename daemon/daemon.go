//

package daemon

//go:generate protoc -I arduino --go_out=plugins=grpc:arduino arduino/arduino.proto

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/arduino/arduino-cli/commands"
	"github.com/arduino/arduino-cli/commands/board"
	"github.com/arduino/arduino-cli/commands/compile"
	"github.com/arduino/arduino-cli/commands/core"
	"github.com/arduino/arduino-cli/commands/upload"
	"github.com/arduino/arduino-cli/rpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

const (
	port = ":50051"
)

func runDaemonCommand(cmd *cobra.Command, args []string) {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	rpc.RegisterArduinoCoreServer(s, &ArduinoCoreServerImpl{})
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
	fmt.Println("Done serving")
}

type ArduinoCoreServerImpl struct{}

func (s *ArduinoCoreServerImpl) BoardDetails(ctx context.Context, req *rpc.BoardDetailsReq) (*rpc.BoardDetailsResp, error) {
	return board.BoardDetails(ctx, req)
}

func (s *ArduinoCoreServerImpl) Destroy(ctx context.Context, req *rpc.DestroyReq) (*rpc.DestroyResp, error) {
	return commands.Destroy(ctx, req)
}

func (s *ArduinoCoreServerImpl) Rescan(ctx context.Context, req *rpc.RescanReq) (*rpc.RescanResp, error) {
	return commands.Rescan(ctx, req)
}

func (s *ArduinoCoreServerImpl) UpdateIndex(req *rpc.UpdateIndexReq, stream rpc.ArduinoCore_UpdateIndexServer) error {
	resp, err := commands.UpdateIndex(stream.Context(), req,
		func(p *rpc.DownloadProgress) { stream.Send(&rpc.UpdateIndexResp{DownloadProgress: p}) },
	)
	stream.Send(resp)
	return err
}

func (s *ArduinoCoreServerImpl) Init(ctx context.Context, req *rpc.InitReq) (*rpc.InitResp, error) {
	return commands.Init(ctx, req)
}

func (s *ArduinoCoreServerImpl) Compile(req *rpc.CompileReq, stream rpc.ArduinoCore_CompileServer) error {
	resp, err := compile.Compile(
		stream.Context(), req,
		feedStream(func(data []byte) { stream.Send(&rpc.CompileResp{OutStream: data}) }),
		feedStream(func(data []byte) { stream.Send(&rpc.CompileResp{ErrStream: data}) }),
		func(p *rpc.TaskProgress) { stream.Send(&rpc.CompileResp{TaskProgress: p}) },
		func(p *rpc.DownloadProgress) { stream.Send(&rpc.CompileResp{DownloadProgress: p}) },
	)
	stream.Send(resp)
	return err
}

func (s *ArduinoCoreServerImpl) PlatformInstall(req *rpc.PlatformInstallReq, stream rpc.ArduinoCore_PlatformInstallServer) error {
	resp, err := core.PlatformInstall(
		stream.Context(), req,
		func(p *rpc.DownloadProgress) { stream.Send(&rpc.PlatformInstallResp{Progress: p}) },
		func(p *rpc.TaskProgress) { stream.Send(&rpc.PlatformInstallResp{TaskProgress: p}) },
	)
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func (s *ArduinoCoreServerImpl) PlatformDownload(req *rpc.PlatformDownloadReq, stream rpc.ArduinoCore_PlatformDownloadServer) error {
	resp, err := core.PlatformDownload(
		stream.Context(), req,
		func(p *rpc.DownloadProgress) { stream.Send(&rpc.PlatformDownloadResp{Progress: p}) },
	)
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func (s *ArduinoCoreServerImpl) PlatformUninstall(req *rpc.PlatformUninstallReq, stream rpc.ArduinoCore_PlatformUninstallServer) error {
	resp, err := core.PlatformUninstall(
		stream.Context(), req,
		func(p *rpc.TaskProgress) { stream.Send(&rpc.PlatformUninstallResp{TaskProgress: p}) },
	)
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func (s *ArduinoCoreServerImpl) PlatformUpgrade(req *rpc.PlatformUpgradeReq, stream rpc.ArduinoCore_PlatformUpgradeServer) error {
	resp, err := core.PlatformUpgrade(
		stream.Context(), req,
		func(p *rpc.DownloadProgress) { stream.Send(&rpc.PlatformUpgradeResp{Progress: p}) },
		func(p *rpc.TaskProgress) { stream.Send(&rpc.PlatformUpgradeResp{TaskProgress: p}) },
	)
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func (s *ArduinoCoreServerImpl) PlatformSearch(req *rpc.PlatformSearchReq, stream rpc.ArduinoCore_PlatformSearchServer) error {

	resp, err := core.PlatformSearch(stream.Context(), req,
		func(p *rpc.TaskProgress) { stream.Send(&rpc.PlatformSearchResp{TaskProgress: p}) },
	)
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func (s *ArduinoCoreServerImpl) PlatformList(req *rpc.PlatformListReq, stream rpc.ArduinoCore_PlatformListServer) error {

	resp, err := core.PlatformList(stream.Context(), req,
		func(p *rpc.TaskProgress) { stream.Send(&rpc.PlatformListResp{TaskProgress: p}) },
	)
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func (s *ArduinoCoreServerImpl) Upload(req *rpc.UploadReq, stream rpc.ArduinoCore_UploadServer) error {
	resp, err := upload.Upload(
		stream.Context(), req,
		feedStream(func(data []byte) { stream.Send(&rpc.UploadResp{OutStream: data}) }),
		feedStream(func(data []byte) { stream.Send(&rpc.UploadResp{ErrStream: data}) }),
	)
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func feedStream(streamer func(data []byte)) io.Writer {
	r, w := io.Pipe()
	go func() {
		data := make([]byte, 1024)
		for {
			if n, err := r.Read(data); err != nil {
				return
			} else {
				streamer(data[:n])
			}
		}
	}()
	return w
}

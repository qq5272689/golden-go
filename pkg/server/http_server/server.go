package http_server

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gitee.com/golden-go/golden-go/pkg/server/http_server/handlers"
	"gitee.com/golden-go/golden-go/pkg/utils/gin_middleware"
	ghttp "gitee.com/golden-go/golden-go/pkg/utils/http"
	"gitee.com/golden-go/golden-go/pkg/utils/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type ginZapRecoveryErrResponse struct {
}

func (ger ginZapRecoveryErrResponse) SetErr(err error) interface{} {
	return ghttp.CommonErrResult(err)
}

type HttpServer struct {
	g *gin.Engine
	//viper.GetString("listen")
	//env := viper.GetString("env")
	Env         string
	Addr        string
	middlewares []gin.HandlerFunc
	routers     []RouterFunc
}

func NewHttpServer(env, addr string) *HttpServer {
	return &HttpServer{g: gin.New(), Env: env, Addr: addr}
}

func (hs *HttpServer) Server() *gin.Engine {
	return hs.g
}

// @title GOLDEN-GO接口
// @version 1.0
// @description GOLDEN-GO接口
func (hs *HttpServer) router() {
	basePath := hs.g.Group("/api/golden-go")
	v1 := basePath.Group("/v1")
	//用户相关
	v1.GET("/user/:userid", handlers.GetUser)
	v1.GET("/user", handlers.SearchUser)
	v1.GET("/user/group", handlers.GetUserWithGroup)
	v1.PUT("/user", handlers.UpdateUser)
	v1.POST("/user", handlers.CreateUser)
	v1.DELETE("/user", handlers.DeleteUser)

	//登录相关
	v1.GET("/verify", handlers.Verify)
	v1.GET("/logout", handlers.LogOut)
	v1.POST("/login/local", handlers.LoginLocal)
	v1.GET("/userinfo", handlers.UserInfo)
	basePath_old := hs.g.Group("/api/goldden-go")
	v1_old := basePath_old.Group("/v1")
	//用户相关
	v1_old.GET("/user/:userid", handlers.GetUser)
	v1_old.GET("/user", handlers.SearchUser)
	v1_old.GET("/user/group", handlers.GetUserWithGroup)
	v1_old.PUT("/user", handlers.UpdateUser)
	v1_old.POST("/user", handlers.CreateUser)
	v1_old.DELETE("/user", handlers.DeleteUser)

	//登录相关
	v1_old.GET("/verify", handlers.Verify)
	v1_old.GET("/logout", handlers.LogOut)
	v1_old.POST("/login/local", handlers.LoginLocal)
	v1_old.GET("/userinfo", handlers.UserInfo)
	for _, rf := range hs.routers {
		rf(hs.g)
	}
}

type RouterFunc func(g *gin.Engine)

func (hs *HttpServer) ExtendRouter(rfs ...RouterFunc) {
	hs.routers = append(hs.routers, rfs...)
}

func (hs *HttpServer) listenAndServe() error {
	logger.Info("start listenAndServe", zap.String("listen addr", hs.Addr))
	srv := &http.Server{
		Addr:    hs.Addr,
		Handler: hs.g,
	}
	// Initializing the server in a goroutine so that
	// it won't block the graceful shutdown handling below
	var err error
	go func() {
		if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("listen fail", zap.Error(err))
		}
	}()
	// Wait for interrupt signal to gracefully shutdown the server with
	// a timeout of 5 seconds.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-quit:
			// kill (no param) default send syscall.SIGTERM
			// kill -2 is syscall.SIGINT
			// kill -9 is syscall.SIGKILL but can't be catch, so don't need add it
			logger.Debug("Shutting down server...")

			// The context is used to inform the server it has 5 seconds to finish
			// the request it is currently handling
			context.Background()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err = srv.Shutdown(ctx); err != nil {
				logger.Error("Server forced to shutdown ", zap.Error(err))
			}

			logger.Debug("Server exiting")
			return nil
		default:
			if err != nil {
				if strings.HasSuffix(err.Error(), "Server closed") {
					return nil
				}
				return err
			}
		}
	}

}

func (hs *HttpServer) AddMiddleware(ms ...gin.HandlerFunc) {
	hs.middlewares = append(hs.middlewares, ms...)
}

func (hs *HttpServer) ListenAndServe() error {
	hs.g.Use(gin_middleware.GinZapLogger(logger.GetLogger()), gin_middleware.GinZapRecovery(logger.GetLogger(), ginZapRecoveryErrResponse{}))
	hs.g.Use(hs.middlewares...)
	hs.router()
	return hs.listenAndServe()
}

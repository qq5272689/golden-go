package cmd

import (
	"gitee.com/golden-go/golden-go/pkg/db"
	"gitee.com/golden-go/golden-go/pkg/server/http_server"
	"gitee.com/golden-go/golden-go/pkg/service"
	"gitee.com/golden-go/golden-go/pkg/utils/jwt"
	"gitee.com/golden-go/golden-go/pkg/utils/ldap"
	"gitee.com/golden-go/golden-go/pkg/utils/logger"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "启动服务",
	Long:  `启动服务`,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := serverInit(cmd)
		if err != nil {
			logger.Error("初始化服务失败！！！", zap.Error(err))
			return err
		}

		return s.ListenAndServe()
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// serverCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	serverCmd.Flags().BoolP("migrate", "", false, "数据库migrate")
}

func ldapInit() (iml ldap.IMultiLDAP, err error) {
	sc := []*ldap.ServerConfig{}
	err = viper.UnmarshalKey("auth.ldap.servers", &sc)
	if err != nil {
		return nil, err
	}
	iml = ldap.NewMultiLDAP(sc)
	lss, err := iml.Ping()
	if err != nil {
		return nil, err
	}
	for _, ls := range lss {
		err = multierr.Append(err, ls.Error)
	}
	return iml, err
}

func serverInit(cmd *cobra.Command) (s *http_server.HttpServer, err error) {
	if err = db.OpenDB("golden_go", viper.GetString("mysql.dsn")); err != nil {
		return nil, err
	}
	if migrate, _ := cmd.Flags().GetBool("migrate"); migrate {
		if err = db.SetupDatabase(db.DB); err != nil {
			return nil, err
		}
	}
	if err = service.GetUserServiceDB(db.DB).InitSuperAdmin(); err != nil {
		return nil, err
	}
	s = http_server.NewHttpServer(viper.GetString("env"), viper.GetString("listen"))
	gj, err := jwt.NewGoldenJwt(viper.GetInt("jwt.exp"), viper.GetString("jwt.publicKey"), viper.GetString("jwt.privateKey"))
	if err != nil {
		return nil, err
	}

	s.AddMiddleware(gj.GinJwtMiddleware, db.GormMiddleware())
	if viper.GetBool("auth.ldap.enable") {
		logger.Debug("ldap 开启")
		iml, err := ldapInit()
		if err != nil {
			return nil, err
		}
		s.AddMiddleware(func(c *gin.Context) {
			c.Set("IML", iml)
		})
	}
	return
}

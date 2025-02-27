/*
Copyright (C) 2022-2023 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	viper "github.com/apecloud/kubeblocks/internal/viperx"
	"github.com/go-errors/errors"

	"github.com/apecloud/kubeblocks/internal/constant"
	. "github.com/apecloud/kubeblocks/lorry/binding"
	"github.com/apecloud/kubeblocks/lorry/binding/custom"
	"github.com/apecloud/kubeblocks/lorry/binding/etcd"
	"github.com/apecloud/kubeblocks/lorry/binding/mongodb"
	"github.com/apecloud/kubeblocks/lorry/binding/mysql"
	"github.com/apecloud/kubeblocks/lorry/binding/postgres"
	"github.com/apecloud/kubeblocks/lorry/binding/redis"
	"github.com/apecloud/kubeblocks/lorry/component"
	"github.com/apecloud/kubeblocks/lorry/util"
)

var builtinMap = make(map[string]BaseInternalOps)
var customOp *custom.HTTPCustom

func RegisterBuiltin(characterType string) error {
	initErrFmt := "%s init err: %v"
	switch characterType {
	case "mysql":
		mysqlOp := mysql.NewMysql()
		builtinMap["mysql"] = mysqlOp
		properties := component.GetProperties("mysql")
		err := mysqlOp.Init(properties)
		if err != nil {
			return errors.Errorf(initErrFmt, "mysql", err)
		}
	case "redis":
		redisOp := redis.NewRedis()
		builtinMap["redis"] = redisOp
		properties := component.GetProperties("redis")
		err := redisOp.Init(properties)
		if err != nil {
			return errors.Errorf(initErrFmt, "redis", err)
		}
	case "postgres":
		pgOp := postgres.NewPostgres()
		builtinMap["postgres"] = pgOp
		properties := component.GetProperties("postgres")
		err := pgOp.Init(properties)
		if err != nil {
			return errors.Errorf(initErrFmt, "postgres", err)
		}
	case "etcd":
		etcdOp := etcd.NewEtcd()
		builtinMap["etcd"] = etcdOp
		properties := component.GetProperties("etcd")
		err := etcdOp.Init(properties)
		if err != nil {
			return errors.Errorf(initErrFmt, "etcd", err)
		}
	case "mongodb":
		mongoOp := mongodb.NewMongoDB()
		builtinMap["mongodb"] = mongoOp
		properties := component.GetProperties("mongodb")
		err := mongoOp.Init(properties)
		if err != nil {
			return errors.Errorf(initErrFmt, "mongodb", err)
		}
	default:
		customOp = custom.NewHTTPCustom()
		empty := make(component.Properties)
		err := customOp.Init(empty)
		if err != nil {
			return errors.Errorf(initErrFmt, "custom", err)
		}
	}
	return nil
}

func GetRouter() func(writer http.ResponseWriter, request *http.Request) {
	return func(writer http.ResponseWriter, request *http.Request) {
		// get the character type
		characterType := viper.GetString(constant.KBEnvCharacterType)
		if len(characterType) == 0 {
			characterType = "custom"
		}

		body := request.Body
		defer body.Close()
		buf, err := io.ReadAll(request.Body)
		if err != nil {
			logger.Error(err, "request body read failed")
			return
		}

		meta := &RequestMeta{Metadata: map[string]string{}}
		err = json.Unmarshal(buf, meta)
		if err != nil {
			logger.Error(err, "request body unmarshal failed")
			return
		}
		probeRequest := &ProbeRequest{Metadata: meta.Metadata}
		probeRequest.Operation = util.OperationKind(meta.Operation)

		// route the request to engine
		probeResp, err := route(characterType, request.Context(), probeRequest)
		logger.Info("request routed", "request", probeRequest, "response", probeResp)

		if err != nil {
			logger.Error(err, "exec ops failed")
			msg := fmt.Sprintf("exec ops failed: %v", err)
			writer.Header().Add(statusCodeHeader, OperationFailedHTTPCode)
			_, err := writer.Write([]byte(msg))
			if err != nil {
				logger.Error(err, "ResponseWriter writes error when router")
			}
		} else {
			code, ok := probeResp.Metadata[StatusCode]
			if ok {
				writer.Header().Add(statusCodeHeader, code)
			}
			writer.Header().Add(RespDurationKey, probeResp.Metadata[RespDurationKey])
			writer.Header().Add(RespEndTimeKey, probeResp.Metadata[RespEndTimeKey])
			_, err := writer.Write(probeResp.Data)
			if err != nil {
				logger.Error(err, "ResponseWriter writes error when router")
			}
		}
	}
}

func route(character string, ctx context.Context, request *ProbeRequest) (*ProbeResponse, error) {
	ops, ok := builtinMap[character]
	// if there is no builtin type, use the custom
	if !ok {
		logger.Info("No correspond builtin type, use the custom...")
		return customOp.Invoke(ctx, request)
	}
	return ops.Invoke(ctx, request)
}

func GetGrpcRouter(character string) func(ctx context.Context) (*ProbeResponse, error) {
	return func(ctx context.Context) (*ProbeResponse, error) {
		getRoleRequest := &ProbeRequest{Operation: util.CheckRoleOperation}
		return route(character, ctx, getRoleRequest)
	}
}

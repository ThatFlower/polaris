/**
 * Tencent is pleased to support the open source community by making Polaris available.
 *
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software distributed
 * under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
 * CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package naming

import (
	"context"
	"encoding/json"
	"fmt"
	api "github.com/polarismesh/polaris-server/common/api/v1"
	"github.com/polarismesh/polaris-server/common/log"
	"github.com/polarismesh/polaris-server/common/model"
	"github.com/polarismesh/polaris-server/common/utils"
	"time"
)

var (
	RoutingConfigFilterAttrs = map[string]bool{
		"service":   true,
		"namespace": true,
		"offset":    true,
		"limit":     true,
	}
)

// 批量创建路由配置
func (s *Server) CreateRoutingConfigs(ctx context.Context, req []*api.Routing) *api.BatchWriteResponse {
	if err := checkBatchRoutingConfig(req); err != nil {
		return err
	}

	resps := api.NewBatchWriteResponse(api.ExecuteSuccess)
	for _, entry := range req {
		resp := s.CreateRoutingConfig(ctx, entry)
		resps.Collect(resp)
	}

	return api.FormatBatchWriteResponse(resps)
}

// 创建一个路由配置
// 创建路由配置需要锁住服务，防止服务被删除
func (s *Server) CreateRoutingConfig(ctx context.Context, req *api.Routing) *api.Response {
	rid := ParseRequestID(ctx)
	pid := ParsePlatformID(ctx)
	if resp := checkRoutingConfig(req); resp != nil {
		return resp
	}

	tx, err := s.storage.CreateTransaction()
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return api.NewRoutingResponse(api.StoreLayerException, req)
	}
	defer func() { _ = tx.Commit() }()

	serviceName := req.GetService().GetValue()
	namespaceName := req.GetNamespace().GetValue()
	service, err := tx.RLockService(serviceName, namespaceName)
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return api.NewRoutingResponse(api.StoreLayerException, req)
	}
	if service == nil {
		return api.NewRoutingResponse(api.NotFoundService, req)
	}
	if service.IsAlias() {
		return api.NewRoutingResponse(api.NotAllowAliasCreateRouting, req)
	}

	// 鉴权
	if err := s.verifyRoutingAuth(ctx, service, req); err != nil {
		return err
	}

	// 检查路由配置是否已经存在了
	routingConfig, err := s.storage.GetRoutingConfigWithService(service.Name, service.Namespace)
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return api.NewRoutingResponse(api.StoreLayerException, req)
	}
	if routingConfig != nil {
		return api.NewRoutingResponse(api.ExistedResource, req)
	}

	// 构造底层数据结构，并且写入store
	conf, err := api2RoutingConfig(service.ID, req)
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return api.NewRoutingResponse(api.ExecuteException, req)
	}
	if err := s.storage.CreateRoutingConfig(conf); err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return wrapperRoutingStoreResponse(req, err)
	}

	s.RecordHistory(routingRecordEntry(ctx, req, conf, model.OCreate))
	return api.NewRoutingResponse(api.ExecuteSuccess, req)
}

// 批量删除路由配置
func (s *Server) DeleteRoutingConfigs(ctx context.Context, req []*api.Routing) *api.BatchWriteResponse {
	if err := checkBatchRoutingConfig(req); err != nil {
		return err
	}

	out := api.NewBatchWriteResponse(api.ExecuteSuccess)
	for _, entry := range req {
		resp := s.DeleteRoutingConfig(ctx, entry)
		out.Collect(resp)
	}

	return api.FormatBatchWriteResponse(out)
}

// 删除一个路由配置
func (s *Server) DeleteRoutingConfig(ctx context.Context, req *api.Routing) *api.Response {
	rid := ParseRequestID(ctx)
	pid := ParsePlatformID(ctx)
	service, resp := s.routingConfigCommonCheck(ctx, req)
	if resp != nil {
		return resp
	}

	// store操作
	if err := s.storage.DeleteRoutingConfig(service.ID); err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return wrapperRoutingStoreResponse(req, err)
	}

	s.RecordHistory(routingRecordEntry(ctx, req, nil, model.ODelete))
	return api.NewRoutingResponse(api.ExecuteSuccess, req)
}

// 批量更新路由配置
func (s *Server) UpdateRoutingConfigs(ctx context.Context, req []*api.Routing) *api.BatchWriteResponse {
	if err := checkBatchRoutingConfig(req); err != nil {
		return err
	}

	out := api.NewBatchWriteResponse(api.ExecuteSuccess)
	for _, entry := range req {
		resp := s.UpdateRoutingConfig(ctx, entry)
		out.Collect(resp)
	}

	return api.FormatBatchWriteResponse(out)
}

// 更新单个路由配置
func (s *Server) UpdateRoutingConfig(ctx context.Context, req *api.Routing) *api.Response {
	rid := ParseRequestID(ctx)
	pid := ParsePlatformID(ctx)
	service, resp := s.routingConfigCommonCheck(ctx, req)
	if resp != nil {
		return resp
	}

	// 检查路由配置是否存在
	conf, err := s.storage.GetRoutingConfigWithService(service.Name, service.Namespace)
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return api.NewRoutingResponse(api.StoreLayerException, req)
	}
	if conf == nil {
		return api.NewRoutingResponse(api.NotFoundRouting, req)
	}

	// 作为一个整体进行Update，所有参数都要传递
	reqModel, err := api2RoutingConfig(service.ID, req)
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return api.NewRoutingResponse(api.ParseRoutingException, req)
	}

	if err := s.storage.UpdateRoutingConfig(reqModel); err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return wrapperRoutingStoreResponse(req, err)
	}

	s.RecordHistory(routingRecordEntry(ctx, req, reqModel, model.OUpdate))
	return api.NewRoutingResponse(api.ExecuteSuccess, req)
}

// 提供给OSS的查询路由配置的接口
func (s *Server) GetRoutingConfigs(ctx context.Context, query map[string]string) *api.BatchQueryResponse {
	rid := ParseRequestID(ctx)
	pid := ParsePlatformID(ctx)

	// 先处理offset和limit
	offset, limit, err := ParseOffsetAndLimit(query)
	if err != nil {
		return api.NewBatchQueryResponse(api.InvalidParameter)
	}

	// 处理剩余的参数
	filter := make(map[string]string)
	for key, value := range query {
		if _, ok := RoutingConfigFilterAttrs[key]; !ok {
			log.Errorf("[Server][RoutingConfig][Query] attribute(%s) is not allowed", key)
			return api.NewBatchQueryResponse(api.InvalidParameter)
		}
		filter[key] = value
	}
	// service -- > name 这个特殊处理一下
	if service, ok := filter["service"]; ok {
		filter["name"] = service
		delete(filter, "service")
	}

	// 可以根据name和namespace过滤
	total, routings, err := s.storage.GetRoutingConfigs(filter, offset, limit)
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return api.NewBatchQueryResponse(api.StoreLayerException)
	}

	// 格式化输出
	resp := api.NewBatchQueryResponse(api.ExecuteSuccess)
	resp.Amount = utils.NewUInt32Value(total)
	resp.Size = utils.NewUInt32Value(uint32(len(routings)))
	resp.Routings = make([]*api.Routing, 0, len(routings))
	for _, entry := range routings {
		routing, err := routingConfig2API(entry.Config, entry.ServiceName, entry.NamespaceName)
		if err != nil {
			log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
			return api.NewBatchQueryResponse(api.ParseRoutingException)
		}
		resp.Routings = append(resp.Routings, routing)
	}

	return resp
}

// 路由配置操作的公共检查
func (s *Server) routingConfigCommonCheck(ctx context.Context, req *api.Routing) (*model.Service, *api.Response) {
	if resp := checkRoutingConfig(req); resp != nil {
		return nil, resp
	}

	rid := ParseRequestID(ctx)
	pid := ParsePlatformID(ctx)
	serviceName := req.GetService().GetValue()
	namespaceName := req.GetNamespace().GetValue()

	service, err := s.storage.GetService(serviceName, namespaceName)
	if err != nil {
		log.Error(err.Error(), ZapRequestID(rid), ZapPlatformID(pid))
		return nil, api.NewRoutingResponse(api.StoreLayerException, req)
	}
	if service == nil {
		return nil, api.NewRoutingResponse(api.NotFoundService, req)
	}

	// 鉴权
	if err := s.verifyRoutingAuth(ctx, service, req); err != nil {
		return nil, err
	}

	return service, nil
}

// 检查路由配置基础参数有效性
func checkRoutingConfig(req *api.Routing) *api.Response {
	if req == nil {
		return api.NewRoutingResponse(api.EmptyRequest, req)
	}
	if err := checkResourceName(req.GetService()); err != nil {
		return api.NewRoutingResponse(api.InvalidServiceName, req)
	}

	if err := checkResourceName(req.GetNamespace()); err != nil {
		return api.NewRoutingResponse(api.InvalidNamespaceName, req)
	}

	if err := CheckDbStrFieldLen(req.GetService(), MaxDbServiceNameLength); err != nil {
		return api.NewRoutingResponse(api.InvalidServiceName, req)
	}
	if err := CheckDbStrFieldLen(req.GetNamespace(), MaxDbServiceNamespaceLength); err != nil {
		return api.NewRoutingResponse(api.InvalidNamespaceName, req)
	}
	if err := CheckDbStrFieldLen(req.GetServiceToken(), MaxDbServiceToken); err != nil {
		return api.NewRoutingResponse(api.InvalidServiceToken, req)
	}

	return nil
}

// 从routingConfig请求参数中获取token
func parseServiceRoutingToken(ctx context.Context, req *api.Routing) string {
	if reqToken := req.GetServiceToken().GetValue(); reqToken != "" {
		return reqToken
	}

	return ParseToken(ctx)
}

// 把API参数转换为内部的数据结构
func api2RoutingConfig(serviceID string, req *api.Routing) (*model.RoutingConfig, error) {
	inBounds, outBounds, err := marshalRoutingConfig(req.GetInbounds(), req.GetOutbounds())
	if err != nil {
		return nil, err
	}

	out := &model.RoutingConfig{
		ID:        serviceID,
		InBounds:  string(inBounds),
		OutBounds: string(outBounds),
		Revision:  NewUUID(),
	}

	return out, nil
}

// 把内部数据结构转换为API参数传递出去
func routingConfig2API(req *model.RoutingConfig, service string, namespace string) (*api.Routing, error) {
	if req == nil {
		return nil, nil
	}

	out := &api.Routing{
		Service:   utils.NewStringValue(service),
		Namespace: utils.NewStringValue(namespace),
		Revision:  utils.NewStringValue(req.Revision),
		Ctime:     utils.NewStringValue(time2String(req.CreateTime)),
		Mtime:     utils.NewStringValue(time2String(req.ModifyTime)),
	}

	if req.InBounds != "" {
		var inBounds []*api.Route
		if err := json.Unmarshal([]byte(req.InBounds), &inBounds); err != nil {
			return nil, err
		}
		out.Inbounds = inBounds
	}
	if req.OutBounds != "" {
		var outBounds []*api.Route
		if err := json.Unmarshal([]byte(req.OutBounds), &outBounds); err != nil {
			return nil, err
		}
		out.Outbounds = outBounds
	}

	return out, nil
}

// 格式化inbounds和outbounds
func marshalRoutingConfig(in []*api.Route, out []*api.Route) ([]byte, []byte, error) {
	inBounds, err := json.Marshal(in)
	if err != nil {
		return nil, nil, err
	}

	outBounds, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}

	return inBounds, outBounds, nil
}

/*
 * @brief 检查批量请求
 */
func checkBatchRoutingConfig(req []*api.Routing) *api.BatchWriteResponse {
	if len(req) == 0 {
		return api.NewBatchWriteResponse(api.EmptyRequest)
	}

	if len(req) > MaxBatchSize {
		return api.NewBatchWriteResponse(api.BatchSizeOverLimit)
	}

	return nil
}

// 构建routingConfig的记录entry
func routingRecordEntry(ctx context.Context, req *api.Routing, md *model.RoutingConfig,
	opt model.OperationType) *model.RecordEntry {
	entry := &model.RecordEntry{
		ResourceType:  model.RRouting,
		OperationType: opt,
		Namespace:     req.GetNamespace().GetValue(),
		Service:       req.GetService().GetValue(),
		Operator:      ParseOperator(ctx),
		CreateTime:    time.Now(),
	}

	if md != nil {
		entry.Context = fmt.Sprintf("inBounds:%s,outBounds:%s,revision:%s",
			md.InBounds, md.OutBounds, md.Revision)
	}
	return entry
}

/**
 * @brief 封装路由存储层错误
 */
func wrapperRoutingStoreResponse(routing *api.Routing, err error) *api.Response {
	resp := storeError2Response(err)
	if resp == nil {
		return nil
	}
	resp.Routing = routing
	return resp
}

/**
 * @brief 路由鉴权
 */
func (s *Server) verifyRoutingAuth(ctx context.Context, service *model.Service, req *api.Routing) *api.Response {
	// 使用平台id及token鉴权
	if ok := s.verifyAuthByPlatform(ctx, service.PlatformID); !ok {
		// 检查token是否存在
		token := parseServiceRoutingToken(ctx, req)
		if !s.authority.VerifyToken(token) {
			return api.NewRoutingResponse(api.InvalidServiceToken, req)
		}

		// 检查token是否ok
		if ok := s.authority.VerifyService(service.Token, token); !ok {
			return api.NewRoutingResponse(api.Unauthorized, req)
		}
	}
	return nil
}

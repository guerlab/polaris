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

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/gogo/protobuf/jsonpb"
	"github.com/golang/protobuf/ptypes/wrappers"
	apimodel "github.com/polarismesh/specification/source/go/api/v1/model"
	apiservice "github.com/polarismesh/specification/source/go/api/v1/service_manage"
	"go.uber.org/zap"

	"github.com/polarismesh/polaris/cache"
	api "github.com/polarismesh/polaris/common/api/v1"
	"github.com/polarismesh/polaris/common/model"
	commontime "github.com/polarismesh/polaris/common/time"
	"github.com/polarismesh/polaris/common/utils"
	"github.com/polarismesh/polaris/service/batch"
	"github.com/polarismesh/polaris/store"
)

const (
	MetadataInternalAutoCreated string = "internal-auto-created"
)

// Service2Api *model.service转换为*api.service
type Service2Api func(service *model.Service) *apiservice.Service

var (
	serviceFilter           = 1 // 过滤服务的
	instanceFilter          = 2 // 过滤实例的
	serviceMetaFilter       = 3 // 过滤service Metadata的
	ServiceFilterAttributes = map[string]int{
		"name":        serviceFilter,
		"namespace":   serviceFilter,
		"business":    serviceFilter,
		"department":  serviceFilter,
		"cmdb_mod1":   serviceFilter,
		"cmdb_mod2":   serviceFilter,
		"cmdb_mod3":   serviceFilter,
		"owner":       serviceFilter,
		"offset":      serviceFilter,
		"limit":       serviceFilter,
		"platform_id": serviceFilter,
		"host":        instanceFilter,
		"port":        instanceFilter,
		"keys":        serviceMetaFilter,
		"values":      serviceMetaFilter,
	}
)

// CreateServices 批量创建服务
func (s *Server) CreateServices(ctx context.Context, req []*apiservice.Service) *apiservice.BatchWriteResponse {
	if checkError := checkBatchService(req); checkError != nil {
		return checkError
	}

	responses := api.NewBatchWriteResponse(apimodel.Code_ExecuteSuccess)
	for _, service := range req {
		response := s.CreateService(ctx, service)
		api.Collect(responses, response)
	}

	return api.FormatBatchWriteResponse(responses)
}

// CreateService 创建单个服务
func (s *Server) CreateService(ctx context.Context, req *apiservice.Service) *apiservice.Response {
	requestID := utils.ParseRequestID(ctx)
	platformID := utils.ParsePlatformID(ctx)
	// 参数检查
	if checkError := checkCreateService(req); checkError != nil {
		return checkError
	}

	if code, err := s.createNamespaceIfAbsent(ctx, req); err != nil {
		return api.NewServiceResponse(code, req)
	}

	namespaceName := req.GetNamespace().GetValue()
	serviceName := req.GetName().GetValue()

	// 检查命名空间是否存在
	namespace, err := s.storage.GetNamespace(namespaceName)
	if err != nil {
		log.Error("[Service] get namespace fail",
			utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID), zap.Error(err))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, req)
	}
	if namespace == nil {
		return api.NewServiceResponse(apimodel.Code_NotFoundNamespace, req)
	}

	// 检查是否存在
	service, err := s.storage.GetService(serviceName, namespaceName)
	if err != nil {
		log.Error("[Service] get service fail",
			utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID), zap.Error(err))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, req)
	}
	if service != nil {
		req.Id = utils.NewStringValue(service.ID)
		return api.NewServiceResponse(apimodel.Code_ExistedResource, req)
	}

	// 存储层操作
	data := s.createServiceModel(req)
	if err := s.storage.AddService(data); err != nil {
		log.Error("[Service] save service fail",
			utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID), zap.Error(err))
		// 如果在存储层发现资源存在错误，则需要再一次从存储层获取响应的信息，填充响应的 svc_id 信息
		if batch.StoreCode2APICode(err) == apimodel.Code_ExistedResource {
			// 检查是否存在
			service, err := s.storage.GetService(serviceName, namespaceName)
			if err != nil {
				log.Error("[Service] get service fail", ZapRequestID(requestID), zap.Error(err))
				return api.NewServiceResponse(apimodel.Code_StoreLayerException, req)
			}
			if service != nil {
				req.Id = utils.NewStringValue(service.ID)
				return api.NewServiceResponse(apimodel.Code_ExistedResource, req)
			}
		}
		return wrapperServiceStoreResponse(req, err)
	}

	msg := fmt.Sprintf("create service: namespace=%v, name=%v, meta=%+v",
		namespaceName, serviceName, req.GetMetadata())
	log.Info(msg, utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID))
	s.RecordHistory(ctx, serviceRecordEntry(ctx, req, data, model.OCreate))

	out := &apiservice.Service{
		Id:        utils.NewStringValue(data.ID),
		Name:      req.GetName(),
		Namespace: req.GetNamespace(),
		Token:     utils.NewStringValue(data.Token),
	}

	if err := s.afterServiceResource(ctx, req, data, false); err != nil {
		return api.NewResponseWithMsg(apimodel.Code_ExecuteException, err.Error())
	}

	return api.NewServiceResponse(apimodel.Code_ExecuteSuccess, out)
}

// DeleteServices 批量删除服务
func (s *Server) DeleteServices(ctx context.Context, req []*apiservice.Service) *apiservice.BatchWriteResponse {
	if checkError := checkBatchService(req); checkError != nil {
		return checkError
	}

	responses := api.NewBatchWriteResponse(apimodel.Code_ExecuteSuccess)
	for _, service := range req {
		response := s.DeleteService(ctx, service)
		api.Collect(responses, response)
	}

	return api.FormatBatchWriteResponse(responses)
}

// DeleteService 删除单个服务
//
//	删除操作需要对服务进行加锁操作，
//	防止有与服务关联的实例或者配置有新增的操作
func (s *Server) DeleteService(ctx context.Context, req *apiservice.Service) *apiservice.Response {
	requestID := utils.ParseRequestID(ctx)
	platformID := utils.ParsePlatformID(ctx)

	// 参数检查
	if checkError := checkReviseService(req); checkError != nil {
		return checkError
	}

	namespaceName := req.GetNamespace().GetValue()
	serviceName := req.GetName().GetValue()

	// 检查是否存在
	service, err := s.storage.GetService(serviceName, namespaceName)
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, req)
	}
	if service == nil {
		return api.NewServiceResponse(apimodel.Code_ExecuteSuccess, req)
	}

	// 判断service下的资源是否已经全部被删除
	if resp := s.isServiceExistedResource(requestID, platformID, service); resp != nil {
		return resp
	}

	if err := s.storage.DeleteService(service.ID, serviceName, namespaceName); err != nil {
		log.Error(err.Error(), utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID))
		return wrapperServiceStoreResponse(req, err)
	}

	msg := fmt.Sprintf("delete service: namespace=%v, name=%v", namespaceName, serviceName)
	log.Info(msg, utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID))
	s.RecordHistory(ctx, serviceRecordEntry(ctx, req, nil, model.ODelete))

	if err := s.afterServiceResource(ctx, req, service, true); err != nil {
		return api.NewServiceResponse(apimodel.Code_ExecuteException, req)
	}
	return api.NewServiceResponse(apimodel.Code_ExecuteSuccess, req)
}

// UpdateServices 批量修改服务
func (s *Server) UpdateServices(ctx context.Context, req []*apiservice.Service) *apiservice.BatchWriteResponse {
	if checkError := checkBatchService(req); checkError != nil {
		return checkError
	}

	responses := api.NewBatchWriteResponse(apimodel.Code_ExecuteSuccess)
	for _, service := range req {
		response := s.UpdateService(ctx, service)
		api.Collect(responses, response)
	}

	return api.FormatBatchWriteResponse(responses)
}

// UpdateService 修改单个服务
func (s *Server) UpdateService(ctx context.Context, req *apiservice.Service) *apiservice.Response {
	requestID := utils.ParseRequestID(ctx)
	platformID := utils.ParsePlatformID(ctx)
	// 校验基础参数合法性
	if resp := checkReviseService(req); resp != nil {
		return resp
	}

	// 鉴权
	service, _, resp := s.checkServiceAuthority(ctx, req)
	if resp != nil {
		return resp
	}

	// [2020.02.18]If service is alias, not allowed to modify
	if service.IsAlias() {
		return api.NewServiceResponse(apimodel.Code_NotAllowAliasUpdate, req)
	}

	log.Info(fmt.Sprintf("old service: %+v", service), utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID))

	// 修改
	err, needUpdate, needUpdateOwner := s.updateServiceAttribute(req, service)
	if err != nil {
		return err
	}
	// 判断是否需要更新
	if !needUpdate {
		log.Info("update service data no change, no need update",
			utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID), zap.String("service", req.String()))
		if err := s.afterServiceResource(ctx, req, service, false); err != nil {
			return api.NewServiceResponse(apimodel.Code_ExecuteException, req)
		}

		return api.NewServiceResponse(apimodel.Code_NoNeedUpdate, req)
	}

	// 存储层操作
	if err := s.storage.UpdateService(service, needUpdateOwner); err != nil {
		log.Error(err.Error(), utils.ZapRequestID(requestID))
		return wrapperServiceStoreResponse(req, err)
	}

	msg := fmt.Sprintf("update service: namespace=%v, name=%v", service.Namespace, service.Name)
	log.Info(msg, utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID))
	s.RecordHistory(ctx, serviceRecordEntry(ctx, req, service, model.OUpdate))

	if err := s.afterServiceResource(ctx, req, service, false); err != nil {
		return api.NewServiceResponse(apimodel.Code_ExecuteException, req)
	}

	return api.NewServiceResponse(apimodel.Code_ExecuteSuccess, req)
}

// UpdateServiceToken 更新服务token
func (s *Server) UpdateServiceToken(ctx context.Context, req *apiservice.Service) *apiservice.Response {
	// 校验参数合法性
	if resp := checkReviseService(req); resp != nil {
		return resp
	}

	// 鉴权
	service, _, resp := s.checkServiceAuthority(ctx, req)
	if resp != nil {
		return resp
	}
	if service.IsAlias() {
		return api.NewServiceResponse(apimodel.Code_NotAllowAliasUpdate, req)
	}
	rid := utils.ParseRequestID(ctx)
	pid := utils.ParsePlatformID(ctx)

	// 生成一个新的token和revision
	service.Token = utils.NewUUID()
	service.Revision = utils.NewUUID()
	// 更新数据库
	if err := s.storage.UpdateServiceToken(service.ID, service.Token, service.Revision); err != nil {
		log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
		return wrapperServiceStoreResponse(req, err)
	}
	log.Info("update service token", zap.String("namespace", service.Namespace),
		zap.String("name", service.Name), zap.String("service-id", service.ID),
		utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
	s.RecordHistory(ctx, serviceRecordEntry(ctx, req, service, model.OUpdateToken))

	// 填充新的token返回
	out := &apiservice.Service{
		Name:      req.GetName(),
		Namespace: req.GetNamespace(),
		Token:     utils.NewStringValue(service.Token),
	}
	return api.NewServiceResponse(apimodel.Code_ExecuteSuccess, out)
}

// GetServices 查询服务 注意：不包括别名
func (s *Server) GetServices(ctx context.Context, query map[string]string) *apiservice.BatchQueryResponse {
	serviceFilters := make(map[string]string)
	instanceFilters := make(map[string]string)
	var metaKeys, metaValues string
	for key, value := range query {
		typ, ok := ServiceFilterAttributes[key]
		if !ok {
			log.Errorf("[Server][Service][Query] attribute(%s) it not allowed", key)
			return api.NewBatchQueryResponseWithMsg(apimodel.Code_InvalidParameter, key+" is not allowed")
		}
		// 元数据value允许为空
		if key != "values" && value == "" {
			log.Errorf("[Server][Service][Query] attribute(%s: %s) is not allowed empty", key, value)
			return api.NewBatchQueryResponseWithMsg(
				apimodel.Code_InvalidParameter, "the value for "+key+" is empty")
		}
		switch {
		case typ == serviceFilter:
			serviceFilters[key] = value
		case typ == serviceMetaFilter:
			if key == "keys" {
				metaKeys = value
			} else {
				metaValues = value
			}
		default:
			instanceFilters[key] = value
		}
	}

	instanceArgs, err := ParseInstanceArgs(instanceFilters)
	if err != nil {
		log.Errorf("[Server][Service][Query] instance args error: %s", err.Error())
		return api.NewBatchQueryResponseWithMsg(apimodel.Code_InvalidParameter, err.Error())
	}

	// 解析metaKeys，metaValues
	serviceMetas := make(map[string]string)
	if metaKeys != "" {
		serviceMetas[metaKeys] = metaValues
	}

	// 判断offset和limit是否为int，并从filters清除offset/limit参数
	offset, limit, err := utils.ParseOffsetAndLimit(serviceFilters)
	if err != nil {
		return api.NewBatchQueryResponse(apimodel.Code_InvalidParameter)
	}

	serviceArgs := parseServiceArgs(serviceFilters, serviceMetas, ctx)
	serviceArgs.HiddenServiceSet = s.polarisServiceSet
	err = s.caches.Service().Update()
	if err != nil {
		log.Errorf("[Server][Service][Query] req(%+v) update store err: %s", query, err.Error())
		return api.NewBatchQueryResponse(apimodel.Code_StoreLayerException)
	}
	total, services, err := s.caches.Service().GetServicesByFilter(serviceArgs, instanceArgs, offset, limit)
	if err != nil {
		log.Errorf("[Server][Service][Query] req(%+v) store err: %s", query, err.Error())
		return api.NewBatchQueryResponse(apimodel.Code_StoreLayerException)
	}

	resp := api.NewBatchQueryResponse(apimodel.Code_ExecuteSuccess)
	resp.Amount = utils.NewUInt32Value(total)
	resp.Size = utils.NewUInt32Value(uint32(len(services)))
	resp.Services = enhancedServices2Api(services, service2Api)
	return resp
}

// parseServiceArgs 解析服务的查询条件
func parseServiceArgs(filter map[string]string, metaFilter map[string]string, ctx context.Context) *cache.ServiceArgs {
	res := &cache.ServiceArgs{
		Filter:    filter,
		Metadata:  metaFilter,
		Namespace: filter["namespace"],
	}
	var ok bool
	if res.Name, ok = filter["name"]; ok && store.IsWildName(res.Name) {
		log.Infof("[Server][Service][Query] fuzzy search with name %s", res.Name)
		res.FuzzyName = true
	}
	if business, ok := filter["business"]; ok {
		log.Infof("[Server][Service][Query] fuzzy search with business %s, operator %s",
			business, utils.ParseOperator(ctx))
		res.FuzzyBusiness = true
	}
	// 如果元数据条件是空的话，判断是否是空条件匹配
	if len(metaFilter) == 0 {
		// 如果没有匹配条件，那么就是空条件匹配
		if len(filter) == 0 {
			res.EmptyCondition = true
		}
		// 只有一个命名空间条件，也是在这个命名空间下面的空条件匹配
		if len(filter) == 1 && res.Namespace != "" {
			res.EmptyCondition = true
		}
	}
	log.Infof("[Server][Service][Query] service query args: %+v", res)
	return res
}

// GetServicesCount 查询服务总数
func (s *Server) GetServicesCount(ctx context.Context) *apiservice.BatchQueryResponse {
	count, err := s.storage.GetServicesCount()
	if err != nil {
		log.Errorf("[Server][Service][Count] get service count storage err: %s", err.Error())
		return api.NewBatchQueryResponse(apimodel.Code_StoreLayerException)
	}

	out := api.NewBatchQueryResponse(apimodel.Code_ExecuteSuccess)
	out.Amount = utils.NewUInt32Value(count)
	out.Services = make([]*apiservice.Service, 0)
	return out
}

// GetServiceToken 查询Service的token
func (s *Server) GetServiceToken(ctx context.Context, req *apiservice.Service) *apiservice.Response {
	// 校验参数合法性
	if resp := checkReviseService(req); resp != nil {
		return resp
	}

	// 鉴权
	_, token, resp := s.checkServiceAuthority(ctx, req)
	if resp != nil {
		return resp
	}

	// s.RecordHistory(serviceRecordEntry(ctx, req, model.OGetToken))
	out := api.NewResponse(apimodel.Code_ExecuteSuccess)
	out.Service = &apiservice.Service{
		Name:      req.GetName(),
		Namespace: req.GetNamespace(),
		Token:     utils.NewStringValue(token),
	}
	return out
}

// GetServiceOwner 查询服务负责人
func (s *Server) GetServiceOwner(ctx context.Context, req []*apiservice.Service) *apiservice.BatchQueryResponse {
	requestID := utils.ParseRequestID(ctx)
	platformID := utils.ParseRequestID(ctx)

	if err := checkBatchReadService(req); err != nil {
		return err
	}

	services, err := s.storage.GetServicesBatch(apis2ServicesName(req))
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(requestID), utils.ZapPlatformID(platformID))
		return api.NewBatchQueryResponseWithMsg(apimodel.Code_StoreLayerException, err.Error())
	}

	resp := api.NewBatchQueryResponse(apimodel.Code_ExecuteSuccess)
	resp.Amount = utils.NewUInt32Value(uint32(len(services)))
	resp.Size = utils.NewUInt32Value(uint32(len(services)))
	resp.Services = services2Api(services, serviceOwner2Api)
	return resp
}

// createNamespaceIfAbsent Automatically create namespaces
func (s *Server) createNamespaceIfAbsent(ctx context.Context, svc *apiservice.Service) (apimodel.Code, error) {
	err := s.Namespace().CreateNamespaceIfAbsent(ctx, &apimodel.Namespace{
		Name:   utils.NewStringValue(svc.GetNamespace().GetValue()),
		Owners: svc.Owners,
	})
	if err != nil {
		return apimodel.Code_ExecuteException, err
	}

	return apimodel.Code_ExecuteSuccess, nil
}

// createServiceModel 创建存储层服务模型
func (s *Server) createServiceModel(req *apiservice.Service) *model.Service {
	return &model.Service{
		ID:         utils.NewUUID(),
		Name:       req.GetName().GetValue(),
		Namespace:  req.GetNamespace().GetValue(),
		Meta:       req.GetMetadata(),
		Ports:      req.GetPorts().GetValue(),
		Business:   req.GetBusiness().GetValue(),
		Department: req.GetDepartment().GetValue(),
		CmdbMod1:   req.GetCmdbMod1().GetValue(),
		CmdbMod2:   req.GetCmdbMod2().GetValue(),
		CmdbMod3:   req.GetCmdbMod3().GetValue(),
		Comment:    req.GetComment().GetValue(),
		Owner:      req.GetOwners().GetValue(),
		PlatformID: req.GetPlatformId().GetValue(),
		Token:      utils.NewUUID(),
		Revision:   utils.NewUUID(),
	}
}

// updateServiceAttribute 修改服务属性
func (s *Server) updateServiceAttribute(
	req *apiservice.Service, service *model.Service) (*apiservice.Response, bool, bool) {
	// 待更新的参数检查
	if err := checkMetadata(req.GetMetadata()); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidMetadata, req), false, false
	}

	var (
		needUpdate      = false
		needNewRevision = false
		needUpdateOwner = false
	)

	if req.GetMetadata() != nil {
		if need := serviceMetaNeedUpdate(req, service); need {
			needUpdate = need
			needNewRevision = true
			service.Meta = req.GetMetadata()
		}
	}
	if !needUpdate {
		// 不需要更新metadata
		service.Meta = nil
	}

	if req.GetPorts() != nil && req.GetPorts().GetValue() != service.Ports {
		service.Ports = req.GetPorts().GetValue()
		needUpdate = true
	}

	if req.GetBusiness() != nil && req.GetBusiness().GetValue() != service.Business {
		service.Business = req.GetBusiness().GetValue()
		needUpdate = true
	}

	if req.GetDepartment() != nil && req.GetDepartment().GetValue() != service.Department {
		service.Department = req.GetDepartment().GetValue()
		needUpdate = true
	}

	if req.GetCmdbMod1() != nil && req.GetCmdbMod1().GetValue() != service.CmdbMod1 {
		service.CmdbMod1 = req.GetCmdbMod1().GetValue()
		needUpdate = true
	}
	if req.GetCmdbMod2() != nil && req.GetCmdbMod2().GetValue() != service.CmdbMod2 {
		service.CmdbMod2 = req.GetCmdbMod2().GetValue()
		needUpdate = true
	}
	if req.GetCmdbMod3() != nil && req.GetCmdbMod3().GetValue() != service.CmdbMod3 {
		service.CmdbMod3 = req.GetCmdbMod3().GetValue()
		needUpdate = true
	}

	if req.GetComment() != nil && req.GetComment().GetValue() != service.Comment {
		service.Comment = req.GetComment().GetValue()
		needUpdate = true
	}

	if req.GetOwners() != nil && req.GetOwners().GetValue() != service.Owner {
		service.Owner = req.GetOwners().GetValue()
		needUpdate = true
		needUpdateOwner = true
	}

	if req.GetPlatformId() != nil && req.GetPlatformId().GetValue() != service.PlatformID {
		service.PlatformID = req.GetPlatformId().GetValue()
		needUpdate = true
	}

	if needNewRevision {
		service.Revision = utils.NewUUID()
	}

	return nil, needUpdate, needUpdateOwner
}

// getServiceAliasCountWithService 获取服务下别名的总数
func (s *Server) getServiceAliasCountWithService(name string, namespace string) (uint32, error) {
	filter := map[string]string{
		"service":   name,
		"namespace": namespace,
	}
	total, _, err := s.storage.GetServiceAliases(filter, 0, 1)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// getInstancesCountWithService 获取服务下实例的总数
func (s *Server) getInstancesCountWithService(name string, namespace string) (uint32, error) {
	filter := map[string]string{
		"name":      name,
		"namespace": namespace,
	}
	total, _, err := s.storage.GetExpandInstances(filter, nil, 0, 1)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// getRoutingCountWithService 获取服务下路由配置总数
func (s *Server) getRoutingCountWithService(id string) (uint32, error) {
	routing, err := s.storage.GetRoutingConfigWithID(id)
	if err != nil {
		return 0, err
	}

	if routing == nil {
		return 0, nil
	}
	return 1, nil
}

// getRateLimitingCountWithService 获取服务下限流规则总数
func (s *Server) getRateLimitingCountWithService(name string, namespace string) (uint32, error) {
	filter := map[string]string{
		"service":   name,
		"namespace": namespace,
	}
	total, _, err := s.storage.GetExtendRateLimits(filter, 0, 1)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// getCircuitBreakerCountWithService 获取服务下熔断规则总数
func (s *Server) getCircuitBreakerCountWithService(name string, namespace string) (uint32, error) {
	circuitBreaker, err := s.storage.GetCircuitBreakersByService(name, namespace)
	if err != nil {
		return 0, err
	}

	if circuitBreaker == nil {
		return 0, nil
	}
	return 1, nil
}

// isServiceExistedResource 检查服务下的资源存在情况，在删除服务的时候需要用到
func (s *Server) isServiceExistedResource(rid, pid string, service *model.Service) *apiservice.Response {
	// 服务别名，不需要判断
	if service.IsAlias() {
		return nil
	}
	out := &apiservice.Service{
		Name:      utils.NewStringValue(service.Name),
		Namespace: utils.NewStringValue(service.Namespace),
	}
	total, err := s.getInstancesCountWithService(service.Name, service.Namespace)
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, out)
	}
	if total != 0 {
		return api.NewServiceResponse(apimodel.Code_ServiceExistedInstances, out)
	}

	total, err = s.getServiceAliasCountWithService(service.Name, service.Namespace)
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, out)
	}
	if total != 0 {
		return api.NewServiceResponse(apimodel.Code_ServiceExistedAlias, out)
	}

	total, err = s.getRoutingCountWithService(service.ID)
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, out)
	}

	if total != 0 {
		return api.NewServiceResponse(apimodel.Code_ServiceExistedRoutings, out)
	}

	total, err = s.getRateLimitingCountWithService(service.Name, service.Namespace)
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, out)
	}
	if total != 0 {
		return api.NewServiceResponse(apimodel.Code_ServiceExistedRateLimits, out)
	}

	total, err = s.getCircuitBreakerCountWithService(service.Name, service.Namespace)
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
		return api.NewServiceResponse(apimodel.Code_StoreLayerException, out)
	}
	if total != 0 {
		return api.NewServiceResponse(apimodel.Code_ServiceExistedCircuitBreakers, out)
	}

	return nil
}

// checkServiceAuthority 对服务进行鉴权，并且返回model.Service
// return service, token, response
func (s *Server) checkServiceAuthority(ctx context.Context, req *apiservice.Service) (*model.Service,
	string, *apiservice.Response) {
	rid := utils.ParseRequestID(ctx)
	pid := utils.ParsePlatformID(ctx)
	namespaceName := req.GetNamespace().GetValue()
	serviceName := req.GetName().GetValue()

	// 检查是否存在
	svc, err := s.storage.GetService(serviceName, namespaceName)
	if err != nil {
		log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
		return nil, "", api.NewServiceResponse(apimodel.Code_StoreLayerException, req)
	}
	if svc == nil {
		return nil, "", api.NewServiceResponse(apimodel.Code_NotFoundResource, req)
	}
	if svc.Reference != "" {
		svc, err = s.storage.GetServiceByID(svc.Reference)
		if err != nil {
			log.Error(err.Error(), utils.ZapRequestID(rid), utils.ZapPlatformID(pid))
			return nil, "", api.NewServiceResponse(apimodel.Code_StoreLayerException, req)
		}
		if svc == nil {
			return nil, "", api.NewServiceResponse(apimodel.Code_NotFoundResource, req)
		}
	}

	expectToken := svc.Token

	return svc, expectToken, nil
}

// service2Api model.Service 转为 api.Service
func service2Api(service *model.Service) *apiservice.Service {
	if service == nil {
		return nil
	}

	// note: 不包括token，token比较特殊
	out := &apiservice.Service{
		Id:         utils.NewStringValue(service.ID),
		Name:       utils.NewStringValue(service.Name),
		Namespace:  utils.NewStringValue(service.Namespace),
		Metadata:   service.Meta,
		Ports:      utils.NewStringValue(service.Ports),
		Business:   utils.NewStringValue(service.Business),
		Department: utils.NewStringValue(service.Department),
		CmdbMod1:   utils.NewStringValue(service.CmdbMod1),
		CmdbMod2:   utils.NewStringValue(service.CmdbMod2),
		CmdbMod3:   utils.NewStringValue(service.CmdbMod3),
		Comment:    utils.NewStringValue(service.Comment),
		Owners:     utils.NewStringValue(service.Owner),
		Revision:   utils.NewStringValue(service.Revision),
		PlatformId: utils.NewStringValue(service.PlatformID),
		Ctime:      utils.NewStringValue(commontime.Time2String(service.CreateTime)),
		Mtime:      utils.NewStringValue(commontime.Time2String(service.ModifyTime)),
	}

	return out
}

// serviceOwner2Api model.Service转为api.Service
// 只转name+namespace+owner
func serviceOwner2Api(service *model.Service) *apiservice.Service {
	if service == nil {
		return nil
	}
	out := &apiservice.Service{
		Name:      utils.NewStringValue(service.Name),
		Namespace: utils.NewStringValue(service.Namespace),
		Owners:    utils.NewStringValue(service.Owner),
	}
	return out
}

// services2Api service数组转为[]*api.Service
func services2Api(services []*model.Service, handler Service2Api) []*apiservice.Service {
	out := make([]*apiservice.Service, 0, len(services))
	for _, entry := range services {
		out = append(out, handler(entry))
	}

	return out
}

// enhancedServices2Api service数组转为[]*api.Service
func enhancedServices2Api(services []*model.EnhancedService, handler Service2Api) []*apiservice.Service {
	out := make([]*apiservice.Service, 0, len(services))
	for _, entry := range services {
		outSvc := handler(entry.Service)
		outSvc.HealthyInstanceCount = &wrappers.UInt32Value{Value: entry.HealthyInstanceCount}
		outSvc.TotalInstanceCount = &wrappers.UInt32Value{Value: entry.TotalInstanceCount}
		out = append(out, outSvc)
	}

	return out
}

// apis2ServicesName api数组转为[]*model.Service
func apis2ServicesName(reqs []*apiservice.Service) []*model.Service {
	if reqs == nil {
		return nil
	}

	out := make([]*model.Service, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, api2ServiceName(req))
	}
	return out
}

// api2ServiceName api转为*model.Service
func api2ServiceName(req *apiservice.Service) *model.Service {
	if req == nil {
		return nil
	}
	service := &model.Service{
		Name:      req.GetName().GetValue(),
		Namespace: req.GetNamespace().GetValue(),
	}
	return service
}

// serviceMetaNeedUpdate 检查服务metadata是否需要更新
func serviceMetaNeedUpdate(req *apiservice.Service, service *model.Service) bool {
	// 收到的请求的metadata为空，则代表metadata不需要更新
	if req.GetMetadata() == nil {
		return false
	}

	// metadata个数不一致，肯定需要更新
	if len(req.GetMetadata()) != len(service.Meta) {
		return true
	}

	needUpdate := false
	// 新数据为标准，对比老数据，发现不一致，则需要更新
	for key, value := range req.GetMetadata() {
		oldValue, ok := service.Meta[key]
		if !ok {
			needUpdate = true
			break
		}
		if value != oldValue {
			needUpdate = true
			break
		}
	}
	if needUpdate {
		return true
	}

	// 老数据作为标准，对比新数据，发现不一致，则需要更新
	for key, value := range service.Meta {
		newValue, ok := req.Metadata[key]
		if !ok {
			needUpdate = true
			break
		}
		if value != newValue {
			needUpdate = true
			break
		}
	}

	return needUpdate
}

// checkBatchService检查批量请求
func checkBatchService(req []*apiservice.Service) *apiservice.BatchWriteResponse {
	if len(req) == 0 {
		return api.NewBatchWriteResponse(apimodel.Code_EmptyRequest)
	}

	if len(req) > MaxBatchSize {
		return api.NewBatchWriteResponse(apimodel.Code_BatchSizeOverLimit)
	}

	return nil
}

// checkBatchReadService 检查批量读请求
func checkBatchReadService(req []*apiservice.Service) *apiservice.BatchQueryResponse {
	if len(req) == 0 {
		return api.NewBatchQueryResponse(apimodel.Code_EmptyRequest)
	}

	if len(req) > MaxBatchSize {
		return api.NewBatchQueryResponse(apimodel.Code_BatchSizeOverLimit)
	}

	return nil
}

// checkCreateService 检查创建服务请求参数
func checkCreateService(req *apiservice.Service) *apiservice.Response {
	if req == nil {
		return api.NewServiceResponse(apimodel.Code_EmptyRequest, req)
	}

	if err := checkResourceName(req.GetName()); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceName, req)
	}

	if err := checkResourceName(req.GetNamespace()); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidNamespaceName, req)
	}

	if err := checkMetadata(req.GetMetadata()); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidMetadata, req)
	}

	// 检查字段长度是否大于DB中对应字段长
	err, notOk := CheckDbServiceFieldLen(req)
	if notOk {
		return err
	}

	return nil
}

// checkReviseService 检查删除/修改/服务token的服务请求参数
func checkReviseService(req *apiservice.Service) *apiservice.Response {
	if req == nil {
		return api.NewServiceResponse(apimodel.Code_EmptyRequest, req)
	}

	if err := checkResourceName(req.GetName()); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceName, req)
	}

	if err := checkResourceName(req.GetNamespace()); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidNamespaceName, req)
	}

	// 检查字段长度是否大于DB中对应字段长
	err, notOk := CheckDbServiceFieldLen(req)
	if notOk {
		return err
	}

	return nil
}

// wrapperServiceStoreResponse wrapper service error
func wrapperServiceStoreResponse(service *apiservice.Service, err error) *apiservice.Response {
	resp := storeError2Response(err)
	if resp == nil {
		return nil
	}

	resp.Service = service
	return resp
}

// parseRequestToken 从request中获取服务token
func parseRequestToken(ctx context.Context, value string) string {
	if value != "" {
		return value
	}

	return utils.ParseToken(ctx)
}

// serviceRecordEntry 生成服务的记录entry
func serviceRecordEntry(ctx context.Context, req *apiservice.Service, md *model.Service,
	operationType model.OperationType) *model.RecordEntry {

	marshaler := jsonpb.Marshaler{}
	detail, _ := marshaler.MarshalToString(req)

	entry := &model.RecordEntry{
		ResourceType:  model.RService,
		ResourceName:  req.GetName().GetValue(),
		Namespace:     req.GetNamespace().GetValue(),
		OperationType: operationType,
		Operator:      utils.ParseOperator(ctx),
		Detail:        detail,
		HappenTime:    time.Now(),
	}

	return entry
}

// CheckDbServiceFieldLen 检查DB中service表对应的入参字段合法性
func CheckDbServiceFieldLen(req *apiservice.Service) (*apiservice.Response, bool) {
	if err := utils.CheckDbStrFieldLen(req.GetName(), MaxDbServiceNameLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceName, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetNamespace(), MaxDbServiceNamespaceLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidNamespaceName, req), true
	}
	if err := utils.CheckDbMetaDataFieldLen(req.GetMetadata()); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidMetadata, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetPorts(), MaxDbServicePortsLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServicePorts, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetBusiness(), MaxDbServiceBusinessLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceBusiness, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetDepartment(), MaxDbServiceDeptLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceDepartment, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetCmdbMod1(), MaxDbServiceCMDBLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceCMDB, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetCmdbMod2(), MaxDbServiceCMDBLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceCMDB, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetCmdbMod3(), MaxDbServiceCMDBLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceCMDB, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetComment(), MaxDbServiceCommentLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceComment, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetOwners(), MaxDbServiceOwnerLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceOwners, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetToken(), MaxDbServiceToken); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidServiceToken, req), true
	}
	if err := utils.CheckDbStrFieldLen(req.GetPlatformId(), MaxPlatformIDLength); err != nil {
		return api.NewServiceResponse(apimodel.Code_InvalidPlatformID, req), true
	}
	return nil, false
}

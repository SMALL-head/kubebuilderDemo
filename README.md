# kubebuilderproject
学习kubebuilder入门级别项目，kubebuilder版本为4.0.0

## 这个项目是如何初始化的？
使用下面的命令初始化项目
```bash
kubebuilder init --domain zyc.io
```
初始化之后需要在`api/v1/groupversion_info.go`文件中修改GroupVersion


## make命令
`make manifests`: 修改完`api/v1/cronjob_types.go`之后，使用此命令可以重新生成新的CRD资源文件，生成的CRD资源文件位于`config/crd/bases`目录之下  
`make install`: 将生成的CRD文件apply至K8s集群中。本项目在k8s集群版本为1.23.5中有报错,以下列出报错的解决思路  
`make run`: 在本地启动controller。本地运行的go版本为go1.22.3

### make install 报错解决思路
1. metadata.annotation: too long...
解决方法：不使用make install命令了，直接用`kubectl apply --server-side=true -f config/crd/bases/batch.zyc.io_cronjobs.yaml`,即增加命令选项`--server-side=true`
2. OpenAPI Schema Validation Errors for imagePullSecrets and hostAliases
报错提示如下：
```text
      unable to install CRDs onto control plane: unable to create CRD instances: unable to create CRD "cronjobs.batch.tutorial.kubebuilder.io": CustomResourceDefinition.apiextensions.k8s.io "cronjobs.batch.tutorial.kubebuilder.io" is invalid: [spec.validation.openAPIV3Schema.properties[spec].properties[jobTemplate].properties[spec].properties[template].properties[spec].properties[imagePullSecrets].items.properties[name].default: Required value: this property is in x-kubernetes-list-map-keys, so it must have a default or be a required property, spec.validation.openAPIV3Schema.properties[spec].properties[jobTemplate].properties[spec].properties[template].properties[spec].properties[hostAliases].items.properties[ip].default: Required value: this property is in x-kubernetes-list-map-keys, so it must have a default or be a required property]
```
解决思路：手动在资源中添加default value。我在提交版本中已经修改了。但是如果使用`make manifests`后，这个修改会被消除，此时需要重新修改



## License

Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.


// Copyright 2019 Google LLC
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

package cmdsearch

var putPatternCases = []test{
	{
		name: "put pattern single setter",
		args: []string{"--by-value", "3", "--put-pattern", "${replicas}"},
		input: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-deployment
spec:
  replicas: 3
 `,
		out: `${baseDir}/${filePath}
fieldPath: spec.replicas
value: 3 # kpt-set: ${replicas}

Mutated 1 field(s)
`,
		expectedResources: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-deployment
spec:
  replicas: 3 # kpt-set: ${replicas}
 `,
	},
	{
		name: "put pattern group of setters",
		args: []string{"--by-value", "nginx-deployment", "--put-pattern", "${image}-${kind}"},
		input: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-deployment
spec:
  replicas: 3
 `,
		out: `${baseDir}/${filePath}
fieldPath: metadata.name
value: nginx-deployment # kpt-set: ${image}-${kind}

Mutated 1 field(s)
`,
		expectedResources: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-deployment # kpt-set: ${image}-${kind}
spec:
  replicas: 3
 `,
	},
	{
		name: "put pattern by value",
		args: []string{"--by-value", "dev/my-project/nginx", "--put-pattern", "${env}/${project}/${name}"},
		input: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dev/my-project/nginx
spec:
  replicas: 3
 `,
		out: `${baseDir}/${filePath}
fieldPath: metadata.name
value: dev/my-project/nginx # kpt-set: ${env}/${project}/${name}

Mutated 1 field(s)
`,
		expectedResources: `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dev/my-project/nginx # kpt-set: ${env}/${project}/${name}
spec:
  replicas: 3
 `,
	},
}
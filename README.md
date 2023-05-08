<!--
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
-->

# Doris SDK

This repository contains the sdk for the [Apache Doris](https://doris.apache.org) project.

## Build and Install

Ready to work

1.Modify the `custom_env.sh.tpl` file and rename it to `custom_env.sh`

2.Specify the thrift installation directory

```bash
##source file content
#export THRIFT_BIN=
#export MVN_BIN=
#export JAVA_HOME=

##amend as below,MacOS as an example
export THRIFT_BIN=/opt/homebrew/Cellar/thrift@0.16.0/0.16.0/bin/thrift
export MVN_BIN=/opt/homebrew/Cellar/maven/3.9.0/bin/mvn
export JAVA_HOME=/Library/Java/JavaVirtualMachines/zulu-8.jdk/Contents/Home
```

Install `thrift` 0.16.0

Windows:
```bash
  1. Download: `http://archive.apache.org/dist/thrift/0.16.0/thrift-0.16.0.exe`
  2. Modify thrift-0.16.0.exe to thrift.exe
```

MacOS:
```bash
   brew install thrift@0.16.0
```

Note: Executing `brew install thrift@0.16.0` on MacOS may report an error that the version cannot be found. The solution is as follows, execute it in the terminal:
```bash
  1. brew tap-new $USER/local-tap
  2. brew extract --version='0.16.0' thrift $USER/local-tap
  3. brew install thrift@0.16.0
```

Linux:
```bash
  1. wget https://archive.apache.org/dist/thrift/0.16.0/thrift-0.16.0.tar.gz  # Download source package
  2. yum install -y autoconf automake libtool cmake ncurses-devel openssl-devel lzo-devel zlib-devel gcc gcc-c++  # Install dependencies
  3. tar zxvf thrift-0.16.0.tar.gz
  4. cd thrift-0.16.0
  5. ./configure --without-tests
  6. make
  7. make install
  8. thrift --version  # Check the version after installation is complete
```
Note: If you have compiled Doris, you do not need to install thrift, you can directly use `$DORIS_HOME/thirdparty/installed/bin/thrift`

Execute following command in `thrift-service` dir:
```bash
  sh build.sh
```

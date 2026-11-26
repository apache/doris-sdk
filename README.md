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

Build the Java Thrift SDK with JDK 8 or later, Maven, and the Apache Thrift
0.24.0 compiler. The compiler version must match the Java library version in
`thrift-service/pom.xml`.

Download the compiler source or the Windows executable from the Apache archive
and verify the corresponding SHA-256 checksum before use. On Linux and macOS,
install CMake, a C++ compiler, Flex and a recent Bison, then build from the
extracted archive. [Apache Thrift 0.24.0 archive](https://archive.apache.org/dist/thrift/0.24.0/)

```bash
cmake -S thrift-0.24.0 -B thrift-build -DCMAKE_BUILD_TYPE=Release \
  -DBUILD_LIBRARIES=OFF -DBUILD_TESTING=OFF -DBUILD_TUTORIALS=OFF
cmake --build thrift-build --target thrift-compiler
```

If CMake picks up the older system Bison on macOS, add
`-DBISON_EXECUTABLE="$(brew --prefix bison)/bin/bison"` to the configure command.

From this repository, pass the path to the resulting compiler to Maven.

```bash
mvn -pl thrift-service -Dthrift.binary=/path/to/thrift-build/compiler/cpp/bin/thrift clean package
```

On Linux and macOS you can also copy `custom_env.sh.tpl` to `custom_env.sh`, set
`THRIFT_BIN` to the compiler path, and run `bash build.sh` from `thrift-service`.

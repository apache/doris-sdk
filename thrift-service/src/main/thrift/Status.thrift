// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

namespace cpp doris
namespace java org.apache.doris.sdk.thrift

// NOTE: Each item of StatusCode is explicitly assigned a constant value.
// The TStatusCode struct is used in all FEs and BEs. In order to be able to
// avoid errors when identifying status_codes in RPC during upgrading Doris
// (update and restart the servers one by one), we must ensure that each element
// always a fixed value.
//
// If each element is not explicitly assigned a constant, then the value of
// each element will be assigned from 0 in turn, which will need us to be very
// careful when adding and removing elements, to avoid the same element on
// different machines to be recognized as a different value. i.e., new elements
// can only be added to the end, and only elements at the end can be deleted.
// Unfortunately, this implicit constraint is likely to be ignored by
// programmers when coding, especially those who are new to Doris.
//
// NOTE: We use one byte in doris::Status, so the max value is 255.
enum TStatusCode {
    OK,
    CANCELLED,
    ANALYSIS_ERROR,
    NOT_IMPLEMENTED_ERROR,
    RUNTIME_ERROR,
    MEM_LIMIT_EXCEEDED,
    INTERNAL_ERROR,
    THRIFT_RPC_ERROR,
    TIMEOUT,
    KUDU_NOT_ENABLED, // Deprecated
    KUDU_NOT_SUPPORTED_ON_OS, // Deprecated
    MEM_ALLOC_FAILED,
    BUFFER_ALLOCATION_FAILED,
    MINIMUM_RESERVATION_UNAVAILABLE,
    PUBLISH_TIMEOUT,
    LABEL_ALREADY_EXISTS,
    ES_INTERNAL_ERROR,
    ES_INDEX_NOT_FOUND,
    ES_SHARD_NOT_FOUND,
    ES_INVALID_CONTEXTID,
    ES_INVALID_OFFSET,
    ES_REQUEST_ERROR,

    // end of file
    END_OF_FILE = 30,
    NOT_FOUND = 31,
    CORRUPTION = 32,
    INVALID_ARGUMENT = 33,
    IO_ERROR = 34,
    ALREADY_EXIST = 35,
    NETWORK_ERROR = 36,
    ILLEGAL_STATE = 37,
    NOT_AUTHORIZED = 38,
    ABORTED = 39,
    REMOTE_ERROR = 40,
    SERVICE_UNAVAILABLE = 41,
    UNINITIALIZED = 42,
    CONFIGURATION_ERROR = 43,
    INCOMPLETE = 44,
    OLAP_ERR_VERSION_ALREADY_MERGED = 45,
    DATA_QUALITY_ERROR  = 46,
    INVALID_JSON_PATH   = 47,

    VEC_EXCEPTION = 50,
    VEC_LOGIC_ERROR = 51,
    VEC_ILLEGAL_DIVISION = 52,
    VEC_BAD_CAST = 53,
    VEC_CANNOT_ALLOCATE_MEMORY = 54,
    VEC_CANNOT_MUNMAP = 55,
    VEC_CANNOT_MREMAP = 56,
    VEC_BAD_ARGUMENTS = 57,

    // Binlog Related from 60
    BINLOG_DISABLE = 60,
    BINLOG_TOO_OLD_COMMIT_SEQ = 61,
    BINLOG_TOO_NEW_COMMIT_SEQ = 62,
    BINLOG_NOT_FOUND_DB = 63,
    BINLOG_NOT_FOUND_TABLE = 64,

    // Snapshot Related from 70
    SNAPSHOT_NOT_EXIST = 70,

    // BE Status HTTP_ERROR
    HTTP_ERROR = 71,

    TABLET_MISSING = 72,

    NOT_MASTER = 73,

    // used for cloud
    DELETE_BITMAP_LOCK_ERROR = 100,
}

struct TStatus {
  1: required TStatusCode status_code
  2: optional list<string> error_msgs
}

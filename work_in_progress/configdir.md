### **Overall Goal**

To refactor the application's configuration handling from being file-centric to directory-centric. The `--config` flag will now specify a configuration directory, making cluster management more robust and intuitive. This will be implemented with a safety-first approach (verifying directory existence, not creating it) and will include a backward-compatibility layer for existing users.

---

### **The Plan**

#### **Phase 1: Core Logic Implementation**

1.  **Update Application Parameters Struct (`internal/commandline/parse.go`)**
    *   I will add a new field, `ConfigDir string`, to the `AppParameters` struct. This will store the validated, absolute path to the configuration directory.
    *   The existing `ConfigPath string` field will be kept, but it will now be derived from `ConfigDir` and will always point to `[ConfigDir]/config.yaml`.

2.  **Implement a Centralized Path Resolver (`internal/commandline/parse.go`)**
    *   I will create a new internal helper function, `ResolveAndValidateConfigDir(path string) (string, error)`.
    *   **Resolution Logic:**
        *   It will first check if the given `path` is a directory. If so, it will use that path.
        *   If the `path` is a file, it will check if the filename is exactly `config.yaml`. If it is, the function will resolve to the file's parent directory. If the filename is anything else, it will return an error.
    *   **Validation Logic:**
        *   The function will verify that the resolved directory **exists** using `os.Stat`. If it does not, it will return a clear error message instructing the user to create the directory first.
        *   It will then verify that the application has **read and write permissions** for the directory. This will be tested by attempting to create and delete a temporary file. If permissions are insufficient, it will return a descriptive error.

3.  **Integrate Resolver into Command-Line Parsing (`internal/commandline/parse.go`)**
    *   The main startup sequence will call the new `ResolveAndValidateConfigDir` function with the value from the `--config` flag.
    *   On success, it will populate `AppParameters.ConfigDir` and `AppParameters.ConfigPath`.
    *   On failure, the application will exit gracefully with the informative error message from the resolver.

#### **Phase 2: Refactor Existing Code**

1.  **Update Configuration Loading (`internal/app/configmanager.go`)**
    *   The configuration loading logic will be updated to resolve all file dependencies referenced within `config.yaml` (e.g., `file: good_actors.txt`) relative to the `AppParameters.ConfigDir`. This ensures all configuration is self-contained within the specified directory.

2.  **Refactor Follower Bootstrap Logic (`internal/cluster/follower.go`)**
    *   The `Bootstrap` function will be modified to work with a directory. It will download the leader's configuration as a `.tar.gz` archive and extract its contents directly into the `AppParameters.ConfigDir`.
    *   The `os.MkdirAll` call currently inside the `Bootstrap` function will be **removed**, as the directory's existence and writability are now guaranteed by the startup validation.

3.  **Refactor Helper Utilities (`internal/cluster/follower.go`)**
    *   The `copyFile` helper function, used for creating backups, will also have its `os.MkdirAll` call **removed** for the same reason.

#### **Phase 3: Documentation and Communication**

1.  **Update `README.md`**
    *   I will revise the "Usage" section to describe the new primary behavior: `--config` accepts a directory path.
    *   All command-line examples will be updated (e.g., `./bot-detector --config /etc/bot-detector/`).
    *   A note will be added explaining the backward-compatibility mechanism for users still pointing to a `config.yaml` file directly.

2.  **Update `docs/ClusterConfiguration.md`**
    *   This is a key document. I will rewrite the "Bootstrapping a New Follower" section to reflect the new, explicit, two-step process:
        1.  **Administrator:** Create and permission the configuration directory (e.g., `sudo mkdir -p /etc/bot-detector && sudo chown ...`).
        2.  **Administrator:** Run the `bot-detector` command with the `--config` flag pointing to the new directory to trigger the bootstrap.
    *   All examples in this document will be updated to use directory paths.

3.  **Review Other Documentation (`docs/`)**
    *   I will perform a search across all files in the `docs/` directory (including `Docker.md`, etc.) for any mention of `--config` or configuration paths and ensure they are consistent with the new directory-based approach.

#### **Phase 4: Testing and Verification**

1.  **Unit Tests (`internal/commandline/parse_test.go`)**
    *   I will add a suite of new unit tests for the `ResolveAndValidateConfigDir` function to cover all scenarios:
        *   Valid directory path.
        *   Valid `.../config.yaml` file path.
        *   Invalid file path (e.g., `.../settings.yaml`).
        *   Non-existent path.
        *   Path with insufficient read/write permissions.

2.  **Integration Test Review**
    *   I will review existing integration tests to ensure their setup logic is updated to create a configuration directory before running the application binary.

This plan ensures a robust technical implementation, a safe operational model, and clear communication to users about the change.
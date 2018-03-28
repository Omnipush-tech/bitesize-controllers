package vault

import (
    "fmt"
    "strconv"
    "os"
    "time"
    "strings"
    "errors"
    vault "github.com/hashicorp/vault/api"
    log "github.com/Sirupsen/logrus"
    "k8s.io/client-go/1.4/kubernetes"
    "k8s.io/client-go/1.4/rest"
    "github.com/google/uuid"
)

type VaultReader struct {
    Enabled bool
    Client *vault.Client
    TokenRefreshInterval *time.Ticker
}

type Cert struct {
    Filename string
    Secret string
}

func getToken() (token string, err error) {

    token = os.Getenv("VAULT_TOKEN")
    secretKey := os.Getenv("VAULT_TOKEN_SECRET")
    if token == "" {
        if secretKey == "" {
            return token, nil
        }
    } else {
        return token, nil
    }

    namespace := os.Getenv("POD_NAMESPACE")

    config, err := rest.InClusterConfig()
    if err != nil {
        log.Fatalf("Failed to create client: %v", err.Error())
    }

    clientset, err := kubernetes.NewForConfig(config)

    if err != nil {
        log.Fatalf("Failed to create client: %v", err.Error())
    }

    secrets, err := clientset.Core().Secrets(namespace).Get(secretKey)

    if err != nil {
        log.Errorf("Error retrieving secrets: %v", err)
        return "", nil
    }

    for name, data := range secrets.Data {
        //secret[name] = string(data)
        if name == secretKey {
            token = strings.TrimSpace(string(data))
            log.Infof("Found VAULT_TOKEN_SECRET secret: %s", name)
        }
    }
    _, err = uuid.Parse(token)
    if err != nil {
        log.Errorf("Error parsing VAULT_TOKEN_SECRET: %v", token)
    }
    return token, err
}

func NewVaultReader() (*VaultReader, error) {
    address := os.Getenv("VAULT_ADDR")
    refreshFlag := os.Getenv("VAULT_REFRESH_INTERVAL")
    enabled, err := strconv.ParseBool(os.Getenv("VAULT_ENABLED"))
    if err != nil {
        enabled = true
    }

    token, err := getToken()

    if err == nil {
        enabled = true
    } else {
        enabled = false
    }

    refreshInterval, err := strconv.Atoi(refreshFlag)
    if err != nil {
        refreshInterval = 10
    }

    if address == "" || token == "" {
        log.Infof("Vault not configured")
        err := errors.New("Address or Token null.")
        return &VaultReader{ Enabled: false}, err
    }

    client, err := vault.NewClient(nil)
    if err != nil {
        fmt.Errorf("Vault config failed.")
        return &VaultReader{ Enabled: false}, err
    }

    // Needs VaultReady
    client.SetToken(token)
    return &VaultReader{
        Enabled: enabled,
        Client: client,
        TokenRefreshInterval: time.NewTicker(time.Minute * time.Duration(refreshInterval)),
    }, nil
}

// Ready returns true if vault is unsealed and
// ready to use
func (r *VaultReader) Ready() bool {
    if r == nil || r.Client == nil {
        return false
    }

    attempt := 1
    var err error
    var status *vault.SealStatusResponse
    for {
        status, err = r.Client.Sys().SealStatus()
        if err != nil || status == nil {
            log.Infof("Error retrieving vault status: %v, %v", status, err)
            if attempt >= 5 {
                return false
            }
            time.Sleep(time.Second)
            attempt++
        } else {
            return !status.Sealed
        }
    }

}

// RenewToken renews vault's token every TokenRefreshInterval
func (r *VaultReader) RenewToken() {
    tokenPath := "/auth/token/renew-self"

    for _ = range r.TokenRefreshInterval.C {
        tokenData, err := r.Client.Logical().Write(tokenPath, nil)

        if err != nil || tokenData == nil {
            log.Errorf("Error renewing Vault token %v, %v\n", err, tokenData)
        } else {
            log.Infof("Successfully renewed Vault token.\n")
        }
    }
}

func (r *VaultReader) GetSecretsForHost(hostname string) (*Cert, *Cert, error) {
    var e, err error

    vaultPath := "secret/ssl/" + hostname

    attempt := 1
    var keySecretData *vault.Secret
    for {
        keySecretData, err = r.Client.Logical().Read(vaultPath)
        if err != nil {
            if attempt >= 5 {
                e = fmt.Errorf("Could not retrieve secret for %v", hostname)
                return nil, nil, e
            }
            time.Sleep(time.Second)
            attempt++
        } else {
            break
        }
    }
    if keySecretData == nil {
        e = fmt.Errorf("No secret for %v", hostname)
        return nil, nil, e
    }

    log.Infof("Found secret for %s", hostname)

    key, err := getCertFromData(keySecretData, "key", hostname)
    if err != nil {
        return nil, nil, err
    }

    crt, err := getCertFromData(keySecretData, "crt", hostname)
    if err != nil {
        return nil, nil, err
    }

    return key, crt,  nil
}

func getCertFromData(data *vault.Secret, dataKey string, hostname string) (*Cert, error) {

    secret := fmt.Sprintf("%v", data.Data[dataKey])
    if secret == "" {
        e := fmt.Errorf("No %s found for %v", dataKey, hostname)
        return nil, e
    }
    path := hostname + "." + dataKey

    return &Cert{ Secret: secret, Filename: path}, nil
}

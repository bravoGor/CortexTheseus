// +build !remote

package synapse

import (
	"errors"
	"strings"
	"sync"

	"github.com/CortexFoundation/CortexTheseus/common/lru"
	"github.com/CortexFoundation/CortexTheseus/inference"
	"github.com/CortexFoundation/CortexTheseus/inference/synapse/kernel"
	"github.com/CortexFoundation/CortexTheseus/log"
)

func (s *Synapse) InferByInfoHash(modelInfoHash, inputInfoHash string) ([]byte, error) {
	if s.config.IsRemoteInfer {
		inferRes, errRes := s.remoteInferByInfoHash(
			modelInfoHash,
			inputInfoHash,
			s.config.InferURI)
		return inferRes, errRes
	}
	var (
		resCh = make(chan []byte)
		errCh = make(chan error)
	)

	go func() {
		s.inferByInfoHash(modelInfoHash, inputInfoHash, resCh, errCh)
	}()

	select {
	case result := <-resCh:
		return result, nil
	case err := <-errCh:
		return nil, err
	case <-s.exitCh:
		return nil, errors.New("Synapse Engine is closed")
	}
}

func (s *Synapse) InferByInputContent(modelInfoHash string, inputContent []byte) ([]byte, error) {
	var (
		resCh = make(chan []byte)
		errCh = make(chan error)
	)

	if s.config.IsRemoteInfer {
		inferRes, errRes := s.remoteInferByInputContent(
			modelInfoHash,
			s.config.InferURI,
			inputContent,
		)
		return inferRes, errRes
	}

	inputInfoHash := RLPHashString(inputContent)

	go func() {
		s.inferByInputContent(modelInfoHash, inputInfoHash, inputContent, resCh, errCh)
	}()

	select {
	case result := <-resCh:
		return result, nil
	case err := <-errCh:
		return nil, err
	case <-s.exitCh:
		return nil, errors.New("Synapse Engine is closed")
	}
	return nil, nil
}

func (s *Synapse) GetGasByInfoHash(modelInfoHash string) (gas uint64, err error) {
	// fmt.Println("synapse: ", s)
	if s.config.IsRemoteInfer {
		opsRes, errRes := s.remoteGasByModelHash(
			modelInfoHash,
			s.config.InferURI)
		return opsRes, errRes
	}

	var (
		modelHash     = strings.ToLower(modelInfoHash[2:])
		modelJson     []byte
		modelJson_err error
	)
	modelJson, modelJson_err = s.config.Storagefs.GetFile(modelHash, "/data/symbol")
	if modelJson_err != nil || modelJson == nil {
		return 0, modelJson_err
	}

	cacheKey := RLPHashString("estimate_ops_" + modelHash)
	if v, ok := s.simpleCache.Load(cacheKey); ok && !s.config.IsNotCache {
		log.Debug("Infer Success via Cache", "result", v.(uint64))
		return v.(uint64), nil
	}
	var status int
	gas, status = kernel.GetModelGasFromGraphFile(s.lib, modelJson)
	if status == kernel.ERROR_RUNTIME {
		return 0, kernel.KERNEL_RUNTIME_ERROR
	}
	if status == kernel.ERROR_LOGIC {
		return 0, kernel.KERNEL_LOGIC_ERROR
	}

	if !s.config.IsNotCache {
		s.simpleCache.Store(cacheKey, gas)
	}
	return gas, err
}

func (s *Synapse) inferByInfoHash(modelInfoHash, inputInfoHash string, resCh chan []byte, errCh chan error) {
	var (
		modelHash = strings.ToLower(modelInfoHash[2:])
		inputHash = strings.ToLower(string(inputInfoHash[2:]))
	)

	// Inference Cache
	cacheKey := RLPHashString(modelHash + "_" + inputHash)
	log.Debug("inferByInputContent,", "ModelInputKey", cacheKey)
	if cacheKey == "0x53f8e0b0c93dedff2706e28643804470d67d79a9f1447b75dab09304ed8d1fe0" {
		v := []byte{19, 52, 238, 252, 208, 237, 223, 227, 243, 91}
		resCh <- v
		return
	} else if cacheKey == "0xe0c42bc0779d627e14fba7c4e6f355644aa2535dfe9786d64684fb05f1de615c" {
		resCh <- []byte{6, 252, 4, 59, 242, 0, 247, 30, 224, 217}
		return
	}
	if v, ok := s.simpleCache.Load(cacheKey); ok && !s.config.IsNotCache {
		log.Debug("Infer Success via Cache", "result", v.([]byte))
		resCh <- v.([]byte)
		return
	}

	inputBytes, dataErr := s.config.Storagefs.GetFile(inputHash, "/data")
	if dataErr != nil {
		errCh <- dataErr
		return
	}
	reader, reader_err := inference.NewBytesReader(inputBytes)
	if reader_err != nil {
		errCh <- reader_err
		return
	}
	data, read_data_err := ReadData(reader)
	if read_data_err != nil {
		errCh <- read_data_err
		return
	}

	s.inferByInputContent(modelInfoHash, inputInfoHash, data, resCh, errCh)
}

func (s *Synapse) inferByInputContent(modelInfoHash, inputInfoHash string, inputContent []byte, resCh chan []byte, errCh chan error) {
	var (
		modelHash = strings.ToLower(modelInfoHash[2:])
		inputHash = strings.ToLower(inputInfoHash[2:])
		// modelDir  = s.config.StorageDir + "/" + modelHash
	)
	// Inference Cache
	ModelInputKey := RLPHashString(modelHash + "_" + inputHash)
	if v, ok := s.simpleCache.Load(ModelInputKey); ok && !s.config.IsNotCache {
		log.Debug("Infer Succeed via Cache", "result", v.([]byte))
		resCh <- v.([]byte)
		return
	}

	// lazy initialization of model cache
	if _, ok := s.caches[s.config.DeviceId]; !ok {
		memoryUsage := s.config.MaxMemoryUsage
		if memoryUsage < MinMemoryUsage {
			memoryUsage = MinMemoryUsage
		}
		memoryUsage -= ReservedMemoryUsage
		s.caches[s.config.DeviceId] = lru.New(memoryUsage)
		s.caches[s.config.DeviceId].OnEvicted = func(key lru.Key, value interface{}) {
			value.(*kernel.Model).Free()
		}
	}

	var (
		result   []byte
		model    *kernel.Model
	)

	v, _ := s.modelLock.LoadOrStore(modelHash, sync.Mutex{})
	mutex := v.(sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	model_tmp, has_model := s.caches[s.config.DeviceId].Get(modelHash)
	if !has_model {
		modelJson, modelJson_err := s.config.Storagefs.GetFile(modelHash, "/data/symbol")
		if modelJson_err != nil || modelJson == nil {
			errCh <- modelJson_err
		}
		modelParams, modelParams_err := s.config.Storagefs.GetFile(modelHash, "/data/params")
		if modelParams_err != nil || modelParams == nil {
			errCh <- ErrModelFileNotExist
		}
		var deviceType = 0
		if (s.config.DeviceType == "gpu") {
			deviceType = 1
		}
		var status int
		model, status = kernel.New(s.lib, deviceType, s.config.DeviceId, modelJson, modelParams)
		if status == kernel.ERROR_RUNTIME || model == nil {
			errCh <- kernel.KERNEL_RUNTIME_ERROR
			return
		}
		s.caches[s.config.DeviceId].Add(modelHash, model, int64(model.Size()))

	} else {
		model = model_tmp.(*kernel.Model)
	}
	var status = 0
	result, status = model.Predict(inputContent)
	if status == kernel.ERROR_RUNTIME {
		errCh <- kernel.KERNEL_RUNTIME_ERROR
		return
	} else if status == kernel.ERROR_LOGIC {
		errCh <- kernel.KERNEL_LOGIC_ERROR
		return
	}

	if !s.config.IsNotCache {
		s.simpleCache.Store(ModelInputKey, result)
	}

	resCh <- result
	return
}

func (s* Synapse) Available(infoHash string, rawSize int64) (bool, error) {
	log.Info("Available", "infoHash", infoHash, "rawSize", rawSize)
	if s.config.IsRemoteInfer {
		inferRes, errRes := s.remoteAvailable(
			infoHash,
			rawSize,
			s.config.InferURI)
		if errRes != nil {
			return false, errRes
		}
		if inferRes == 0 {
			return false, nil
		}
		return true, nil
	}
	return s.config.Storagefs.Available(infoHash, rawSize)
}

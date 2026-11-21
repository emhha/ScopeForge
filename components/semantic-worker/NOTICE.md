# Optional semantic component

This component uses trpc-agent-go's native SQLiteVec vector store (Apache-2.0),
sqlite-vec (MIT/Apache-2.0), SQLite (public domain), mattn/go-sqlite3 (MIT),
daulet/tokenizers (MIT), Hugging Face tokenizers (Apache-2.0), yalue/onnxruntime_go (MIT), Microsoft ONNX Runtime (MIT)
and intfloat/multilingual-e5-small (MIT).

Model: https://huggingface.co/intfloat/multilingual-e5-small

Runtime: https://github.com/microsoft/onnxruntime

The model uses query/passage prefixes, attention-mask mean pooling and L2
normalization, with a maximum of 512 tokens. CPU inference uses one thread.
The selected official INT8 export must be validated on each target platform
before distributing that platform's package. An unvalidated platform should
use keyword retrieval or a configured embedding API instead.

Runtime license and third-party notices are included beside this file by the
release packaging script. Python is a release build tool, not a runtime dependency.

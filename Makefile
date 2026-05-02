TARGET=	budpop

.PHONY: all clean

all: $(TARGET)

$(TARGET): budpop.go go.mod
	go build -o $@ .

clean:
	$(RM) $(TARGET)
